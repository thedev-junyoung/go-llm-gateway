// Basic example: single OpenAI provider, one chat completion.
//
// Run:
//
//	OPENAI_API_KEY=sk-... go run ./examples/basic
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

	// Caller's context is the only time budget (ADR-004 Q4).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Correlate logs / metrics across the gateway internals.
	ctx = gateway.WithRequestID(ctx, "req_basic_demo")

	maxTokens := 128
	resp, err := gw.Chat(ctx, provider.ChatRequest{
		Model:     "gpt-4o-mini",
		System:    "You answer in one short sentence.",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "Why is the sky blue?"}},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		handleErr(err)
		return 1
	}

	fmt.Println(resp.Content)
	slog.Info("done",
		"finish_reason", resp.FinishReason,
		"input_tokens", resp.Usage.InputTokens,
		"output_tokens", resp.Usage.OutputTokens,
	)
	return 0
}

// handleErr demonstrates the sentinel error pattern callers use to branch
// on router-critical failures without parsing vendor strings.
func handleErr(err error) {
	var pe *provider.ProviderError
	switch {
	case errors.Is(err, provider.ErrRateLimited):
		if errors.As(err, &pe) && pe.RetryAfter != nil {
			slog.Error("rate limited", "retry_after", *pe.RetryAfter)
		} else {
			slog.Error("rate limited", "vendor", peVendor(err))
		}
	case errors.Is(err, provider.ErrAuthFailed):
		slog.Error("auth failed — check API key", "vendor", peVendor(err))
	case errors.Is(err, provider.ErrTimeout):
		slog.Error("timeout — caller ctx expired or vendor slow", "err", err)
	default:
		slog.Error("chat failed", "err", err)
	}
}

func peVendor(err error) string {
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		return pe.Vendor()
	}
	return "unknown"
}
