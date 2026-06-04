package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

const (
	// streamBufferSize matches the OpenAI adapter — see ADR-007 Q6
	// rationale for why this stays tight rather than ballooning.
	streamBufferSize = 16
)

// wireStreamRequest reuses the standard request shape and adds stream:
// true. Anthropic emits usage inline (input_tokens on message_start,
// output_tokens on message_delta) so there's no include_usage flag
// equivalent to OpenAI's stream_options.
type wireStreamRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"`
	Stream    bool          `json:"stream"`
}

// streamEvent is the SSE event envelope. Anthropic uses event-based SSE
// (`event: <name>\ndata: <json>`), so we accumulate the event name and
// payload across consecutive lines until the dispatching blank line.
type streamEvent struct {
	name string
	data []byte
}

// wireStreamMessageStart carries the initial usage.input_tokens. Output
// is zero here and arrives later on message_delta.
type wireStreamMessageStart struct {
	Message struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// wireStreamContentBlockDelta carries one text delta within a content
// block. Only text_delta is decoded; other delta types (tool_use,
// thinking, ...) flow through Raw on the v0.2 escape hatch — currently
// ignored as per ADR-007 Q3 deferral.
type wireStreamContentBlockDelta struct {
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

// wireStreamMessageDelta carries the terminal stop_reason and the
// finalized usage.output_tokens.
type wireStreamMessageDelta struct {
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// wireStreamError mirrors the non-streaming error envelope — Anthropic
// uses the same `{error: {type, message}}` shape for both pre-stream
// HTTP errors and mid-stream `event: error` frames.
type wireStreamError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ChatStream issues a streaming Messages request. The producer
// goroutine accumulates input_tokens from message_start and
// output_tokens from message_delta, emits one StreamChunk per
// content_block_delta, and emits a terminal chunk on message_stop
// (or on mid-stream error per ADR-007 Q7).
//
// CONTRACT: caller MUST defer cancel() on the ctx passed in. The
// producer watches ctx.Done(); leaving the range loop without
// cancelling leaves the producer parked. See ADR-007 Q6 / Risks.
func (c *Client) ChatStream(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	if !c.SupportsModel(req.Model) {
		return nil, provider.NewProviderError(
			vendorName, provider.ErrorTypeInvalidInput, 0, false,
			fmt.Sprintf("unsupported model %q", req.Model), nil)
	}

	maxTokens, err := c.resolveMaxTokens(req.MaxTokens)
	if err != nil {
		return nil, err
	}

	msgs := make([]wireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, wireMessage{Role: string(m.Role), Content: m.Content})
	}

	body, err := json.Marshal(wireStreamRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
		System:    req.System,
		Messages:  msgs,
		Stream:    true,
	})
	if err != nil {
		return nil, provider.NewProviderError(
			vendorName, provider.ErrorTypeUnknown, 0, false, "encode request", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, provider.NewProviderError(
			vendorName, provider.ErrorTypeUnknown, 0, false, "build http request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", c.apiVersion)
	httpReq.Header.Set("Accept", "text/event-stream")

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, mapTransportError(ctx, err)
	}

	if httpResp.StatusCode >= 400 {
		defer func() { _ = httpResp.Body.Close() }()
		raw, _ := io.ReadAll(httpResp.Body)
		return nil, mapHTTPError(httpResp, raw)
	}

	out := make(chan provider.StreamChunk, streamBufferSize)
	go c.consumeStream(ctx, httpResp, out)
	return out, nil
}

// consumeStream is the producer goroutine. Owns the HTTP response body
// and the output channel; both clean up on return.
func (c *Client) consumeStream(ctx context.Context, resp *http.Response, out chan<- provider.StreamChunk) {
	defer close(out)
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var usage provider.Usage
	var stopReason string
	var current streamEvent

	for scanner.Scan() {
		if ctx.Err() != nil {
			return // Q6: ctx cancel/deadline → close only
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			// Blank line dispatches the event. SSE permits multiple
			// data lines per event, but Anthropic's stream uses one
			// data line per event, so a single accumulator is enough.
			if current.name == "" || len(current.data) == 0 {
				current = streamEvent{}
				continue
			}
			if done := c.dispatchEvent(current, &usage, &stopReason, out); done {
				return
			}
			current = streamEvent{}
			continue
		}

		switch {
		case bytes.HasPrefix(line, []byte("event: ")):
			current.name = string(bytes.TrimPrefix(line, []byte("event: ")))
		case bytes.HasPrefix(line, []byte("data: ")):
			current.data = append(current.data[:0], bytes.TrimPrefix(line, []byte("data: "))...)
		case line[0] == ':':
			// SSE comment / keep-alive — ignore.
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		out <- provider.StreamChunk{
			FinishReason: provider.FinishUnknown,
			Err:          mapTransportError(ctx, err),
		}
	}
}

// dispatchEvent processes one accumulated SSE event. Returns true when
// the stream should terminate (message_stop, error frame, or a parse
// failure on a structural event). State accumulators (usage, stopReason)
// are pointer arguments so this function stays a pure dispatch — the
// consumeStream loop owns the lifecycle.
func (c *Client) dispatchEvent(ev streamEvent, usage *provider.Usage, stopReason *string,
	out chan<- provider.StreamChunk,
) bool {
	switch ev.name {
	case "message_start":
		var p wireStreamMessageStart
		if err := json.Unmarshal(ev.data, &p); err == nil {
			usage.InputTokens = p.Message.Usage.InputTokens
			// message_start's output_tokens is 0 but message_delta may
			// also be skipped on early termination — accept either.
			usage.OutputTokens = p.Message.Usage.OutputTokens
		}
	case "content_block_delta":
		var p wireStreamContentBlockDelta
		if err := json.Unmarshal(ev.data, &p); err != nil {
			out <- provider.StreamChunk{
				FinishReason: provider.FinishUnknown,
				Err: provider.NewProviderError(vendorName, provider.ErrorTypeUnknown,
					0, false, "decode content_block_delta", err),
			}
			return true
		}
		if p.Delta.Type == "text_delta" && p.Delta.Text != "" {
			out <- provider.StreamChunk{ContentDelta: p.Delta.Text}
		}
	case "message_delta":
		var p wireStreamMessageDelta
		if err := json.Unmarshal(ev.data, &p); err == nil {
			if p.Delta.StopReason != "" {
				*stopReason = p.Delta.StopReason
			}
			if p.Usage.OutputTokens > 0 {
				usage.OutputTokens = p.Usage.OutputTokens
			}
		}
	case "message_stop":
		// TODO(v0.2): populate Raw with the original message_stop
		// event so tool_use / multi-modal callers can inspect the
		// vendor payload (ADR-007 Q3 escape hatch). Deferred to
		// keep PR-3 focused on the text path.
		out <- provider.StreamChunk{
			FinishReason: mapStopReason(*stopReason),
			Usage:        *usage,
		}
		return true
	case "error":
		var p wireStreamError
		_ = json.Unmarshal(ev.data, &p) // tolerate non-JSON
		msg := p.Error.Message
		if msg == "" {
			msg = "anthropic stream error"
		}
		typ := provider.ErrorTypeServer
		retriable := true
		if p.Error.Type == "overloaded_error" {
			typ = provider.ErrorTypeOverloaded
		}
		out <- provider.StreamChunk{
			FinishReason: provider.FinishUnknown,
			Err:          provider.NewProviderError(vendorName, typ, 0, retriable, msg, nil),
		}
		return true
	}
	// content_block_start / content_block_stop / ping → no state change,
	// no chunk emission. Continue.
	return false
}

// Compile-time assertion that *Client satisfies StreamingProvider.
var _ provider.StreamingProvider = (*Client)(nil)
