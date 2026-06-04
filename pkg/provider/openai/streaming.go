package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

const (
	// streamBufferSize sizes the chunk channel. Small — SSE backpressure
	// (vendor faster than UI consumer) is rare and a deeper buffer just
	// hides slow consumers. ADR-007 Q6 producer goroutine relies on
	// caller ranging the channel; a tighter buffer surfaces consumer
	// stalls faster.
	streamBufferSize = 16

	// sseDoneMarker is OpenAI's literal end-of-stream sentinel sent on
	// the last `data:` line. Detected before JSON-decoding because it
	// isn't valid JSON.
	sseDoneMarker = "[DONE]"
)

// wireStreamRequest adds the streaming flags to the standard request.
// stream_options.include_usage = true so the final chunk carries usage —
// OpenAI omits it otherwise (Anthropic and Gemini emit usage inline by
// default). Field ordering matches wireRequest so the diff against the
// sync path stays small.
type wireStreamRequest struct {
	Model         string             `json:"model"`
	Messages      []wireMessage      `json:"messages"`
	MaxTokens     *int               `json:"max_tokens,omitempty"`
	Stream        bool               `json:"stream"`
	StreamOptions *wireStreamOptions `json:"stream_options,omitempty"`
}

type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// wireStreamChunk parses one `data: {...}` SSE event. Empty fields are
// common (role-only first event, usage-only terminal event), so all
// fields are tolerant of zero values.
type wireStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// ChatStream issues a streaming Chat Completions request. The returned
// channel emits one provider.StreamChunk per non-empty SSE event, plus
// a terminal chunk carrying FinishReason and Usage. On a mid-stream
// failure the terminal chunk's Err is non-nil and the channel closes.
//
// CONTRACT: caller MUST defer cancel() on the ctx passed in. The
// producer goroutine watches ctx.Done(); breaking out of the range
// loop without cancelling leaves the producer parked on a channel send
// until the underlying HTTP transport eventually returns. See ADR-007
// Q6 / Risks.
func (c *Client) ChatStream(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	if !c.SupportsModel(req.Model) {
		return nil, provider.NewProviderError(
			vendorName, provider.ErrorTypeInvalidInput, 0, false,
			fmt.Sprintf("unsupported model %q", req.Model), nil)
	}

	msgs := make([]wireMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, wireMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, wireMessage{Role: string(m.Role), Content: m.Content})
	}

	body, err := json.Marshal(wireStreamRequest{
		Model:         req.Model,
		Messages:      msgs,
		MaxTokens:     req.MaxTokens,
		Stream:        true,
		StreamOptions: &wireStreamOptions{IncludeUsage: true},
	})
	if err != nil {
		return nil, provider.NewProviderError(
			vendorName, provider.ErrorTypeUnknown, 0, false, "encode request", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, provider.NewProviderError(
			vendorName, provider.ErrorTypeUnknown, 0, false, "build http request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, mapTransportError(ctx, err)
	}

	// Pre-stream HTTP error: the entire error envelope is in the body,
	// so eagerly read + map and return without channel allocation
	// (ADR-007 Q4 — pre-stream failover allowed; nil channel is fine
	// here because we return an error too).
	if httpResp.StatusCode >= 400 {
		defer func() { _ = httpResp.Body.Close() }()
		raw, _ := io.ReadAll(httpResp.Body)
		return nil, mapHTTPError(httpResp, raw)
	}

	out := make(chan provider.StreamChunk, streamBufferSize)
	go c.consumeStream(ctx, httpResp, out)
	return out, nil
}

// consumeStream is the producer goroutine. It owns the HTTP response
// body and the output channel — both are cleaned up on return. ADR-007
// Q6: ctx.Done() observation is delegated to the underlying ctx-bound
// http.Request, which surfaces as scanner.Err on cancel. Q7: any post-
// dispatch failure emits one terminal chunk with Err set, then closes.
func (c *Client) consumeStream(ctx context.Context, resp *http.Response, out chan<- provider.StreamChunk) {
	defer close(out)
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	// Default Scanner buffer is 64 KiB. A single SSE chunk should easily
	// fit, but vendors occasionally batch usage + delta into one larger
	// frame. Bump to 1 MiB so we never silently truncate.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lastFinishReason string
	var lastUsage provider.Usage

	for scanner.Scan() {
		if ctx.Err() != nil {
			return // Q6: ctx cancel → close (no Err chunk)
		}

		line := scanner.Bytes()
		// SSE keep-alive comments start with ':'. Blank lines separate
		// events. Anything not prefixed with `data:` is skipped.
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		data, ok := bytes.CutPrefix(line, []byte("data: "))
		if !ok {
			continue
		}
		payload := strings.TrimSpace(string(data))
		if payload == sseDoneMarker {
			// Emit terminal chunk with the finish_reason + usage we
			// accumulated, then return (channel closes via defer).
			// TODO(v0.2): populate Raw with the original [DONE]-preceding
			// event so tool_use / multi-modal callers can access the
			// vendor payload (ADR-007 Q3 escape hatch). Deferred to
			// keep PR-2 focused on the text path.
			out <- provider.StreamChunk{
				FinishReason: mapFinishReason(lastFinishReason),
				Usage:        lastUsage,
			}
			return
		}

		var chunk wireStreamChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			out <- provider.StreamChunk{
				FinishReason: provider.FinishUnknown,
				Err: provider.NewProviderError(vendorName, provider.ErrorTypeUnknown,
					resp.StatusCode, false, "decode stream chunk", err),
			}
			return
		}

		// Usage-only frame (no choices, just usage) is OpenAI's
		// include_usage=true contract. Capture, don't emit yet —
		// the [DONE] marker triggers the terminal chunk.
		if chunk.Usage != nil {
			lastUsage = provider.Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		first := chunk.Choices[0]
		if first.FinishReason != "" {
			lastFinishReason = first.FinishReason
		}
		// Only emit a chunk when there's actual delta content. Pure
		// metadata frames (role-only first event, finish-reason-only)
		// just update local state — ADR-007 Q3's ContentDelta-empty
		// chunks are reserved for the terminal frame.
		if first.Delta.Content != "" {
			out <- provider.StreamChunk{ContentDelta: first.Delta.Content}
		}
	}

	// Q6: ctx cancel / deadline → channel close only (no Err chunk).
	// Anything else (vendor closed connection, network reset) is a real
	// mid-stream failure → Q7 Err chunk. ctx.Err() captures both
	// context.Canceled and context.DeadlineExceeded; checking that
	// instead of errors.Is(err, context.Canceled) closes the gap where
	// a WithTimeout ctx expiring leaked an Err chunk in violation of Q6.
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		out <- provider.StreamChunk{
			FinishReason: provider.FinishUnknown,
			Err:          mapTransportError(ctx, err),
		}
	}
}

// Compile-time assertion that *Client satisfies StreamingProvider.
var _ provider.StreamingProvider = (*Client)(nil)
