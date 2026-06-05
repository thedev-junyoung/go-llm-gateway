// Streaming example: single OpenAI provider, ChatStream with typewriter
// UX. Demonstrates the ADR-007 contract — defer cancel() on the ctx,
// range the channel, branch on chunk.Err.
//
// Run:
//
//	OPENAI_API_KEY=sk-... go run ./examples/streaming
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	gateway "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider/openai"
)

func main() { os.Exit(run()) }

// run is split out so deferred cleanup actually fires before exit —
// os.Exit inside main skips defers (gocritic: exitAfterDefer).
func run() int {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		slog.Error("OPENAI_API_KEY is required")
		return 2
	}

	gw, err := gateway.New(gateway.Config{
		Providers: []provider.Provider{
			openai.New(key),
		},
	})
	if err != nil {
		slog.Error("gateway init failed", "err", err)
		return 1
	}

	// ADR-007 CONTRACT: caller MUST defer cancel() on the ctx passed
	// to ChatStream. Leaving the range loop without cancel can park
	// the producer goroutine on a send until the underlying HTTP
	// transport times out — a leak only visible under load.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Correlate logs / metrics across the gateway internals.
	ctx = gateway.WithRequestID(ctx, "req_streaming_demo")

	maxTokens := 256
	stream, err := gw.ChatStream(ctx, provider.ChatRequest{
		Model:     "gpt-4o-mini",
		System:    "You explain things briefly, one paragraph max.",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "Why is the sky blue?"}},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		handlePreStreamErr(err)
		return 1
	}

	// Typewriter UX: print each ContentDelta to stdout as it arrives.
	// The first chunk's arrival time is the user-perceived latency
	// (TTFT) — much faster than waiting for the full response.
	start := time.Now()
	var (
		ttft         time.Duration
		streamErr    error
		finishReason provider.FinishReason
		usage        provider.Usage
	)
	for chunk := range stream {
		if chunk.Err != nil {
			// ADR-007 Q7: mid-stream errors arrive as a terminal
			// chunk with Err set. The channel is closed right after.
			streamErr = chunk.Err
			continue
		}
		if ttft == 0 && chunk.ContentDelta != "" {
			ttft = time.Since(start)
		}
		if chunk.ContentDelta != "" {
			fmt.Print(chunk.ContentDelta)
		}
		// Terminal chunk carries FinishReason + Usage; intermediate
		// content chunks leave them at zero value.
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
		if chunk.Usage.InputTokens > 0 || chunk.Usage.OutputTokens > 0 {
			usage = chunk.Usage
		}
	}
	fmt.Println() // newline after the streamed text

	if streamErr != nil {
		handleMidStreamErr(streamErr, ttft)
		return 1
	}

	slog.Info("stream done",
		"ttft_ms", ttft.Milliseconds(),
		"total_ms", time.Since(start).Milliseconds(),
		"finish_reason", finishReason,
		"input_tokens", usage.InputTokens,
		"output_tokens", usage.OutputTokens,
	)
	return 0
}

// handlePreStreamErr branches on the sentinel error pattern for errors
// returned BEFORE the stream channel was allocated (router failure,
// auth, immediate vendor reject). Same shape as Chat's error path.
func handlePreStreamErr(err error) {
	var pe *provider.ProviderError
	switch {
	case errors.Is(err, provider.ErrAuthFailed):
		slog.Error("auth failed — check API key", "vendor", peVendor(err))
	case errors.Is(err, provider.ErrRateLimited):
		if errors.As(err, &pe) && pe.RetryAfter != nil {
			slog.Error("rate limited", "retry_after", *pe.RetryAfter)
		} else {
			slog.Error("rate limited", "vendor", peVendor(err))
		}
	default:
		slog.Error("pre-stream failure", "err", err)
	}
}

// handleMidStreamErr distinguishes mid-stream failures — by this point
// some content may have already reached the user. ttft tells us
// whether the typewriter UX got any text out before the stream broke.
func handleMidStreamErr(err error, ttft time.Duration) {
	slog.Error("mid-stream failure",
		"err", err,
		"ttft_ms", ttft.Milliseconds(),
		"partial_output_shown", ttft > 0,
	)
}

func peVendor(err error) string {
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		return pe.Vendor()
	}
	return "unknown"
}
