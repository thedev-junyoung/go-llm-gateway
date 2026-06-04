package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

const (
	// streamBufferSize matches the other adapters — see ADR-007 Q6
	// for the rationale on a tight buffer surfacing slow consumers.
	streamBufferSize = 16
)

// wireStreamChunk mirrors the shape of a single SSE event from
// streamGenerateContent. Gemini emits the same envelope as the
// non-streaming endpoint, just delivered one chunk per delta.
//
// usageMetadata is cumulative — every chunk carries the running totals,
// so the producer just overwrites on each chunk and the last value
// wins. finishReason is non-empty only on the terminal chunk.
type wireStreamChunk struct {
	Candidates    []wireCandidate    `json:"candidates"`
	UsageMetadata *wireUsageMetadata `json:"usageMetadata"`
}

// ChatStream issues a streamGenerateContent request with alt=sse so
// the response is line-oriented `data: <json>` SSE rather than the
// default JSON-array stream (which would require an incremental
// JSON parser).
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

	contents := make([]wireContent, 0, len(req.Messages))
	for _, m := range req.Messages {
		contents = append(contents, wireContent{
			Role:  mapRoleToWire(m.Role),
			Parts: []wirePart{{Text: m.Content}},
		})
	}

	wreq := wireRequest{Contents: contents}
	if req.System != "" {
		wreq.SystemInstruction = &wireContent{
			Parts: []wirePart{{Text: req.System}},
		}
	}
	if req.MaxTokens != nil {
		if *req.MaxTokens <= 0 {
			return nil, provider.NewProviderError(
				vendorName, provider.ErrorTypeInvalidInput, 0, false,
				"MaxTokens must be > 0", nil)
		}
		wreq.GenerationConfig = &wireGenerationConfig{MaxOutputTokens: *req.MaxTokens}
	}

	body, err := json.Marshal(wreq)
	if err != nil {
		return nil, provider.NewProviderError(
			vendorName, provider.ErrorTypeUnknown, 0, false, "encode request", err)
	}

	endpoint := c.baseURL + "/models/" + url.PathEscape(req.Model) + ":streamGenerateContent?alt=sse"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, provider.NewProviderError(
			vendorName, provider.ErrorTypeUnknown, 0, false, "build http request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", c.apiKey)
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
//
// Gemini has no [DONE] marker — the SSE stream just ends when the
// server closes the connection. The producer accumulates usage and
// finish reason across chunks, then emits a single terminal chunk
// before close on EOF.
func (c *Client) consumeStream(ctx context.Context, resp *http.Response, out chan<- provider.StreamChunk) {
	defer close(out)
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var usage provider.Usage
	var finishReason string

	for scanner.Scan() {
		if ctx.Err() != nil {
			return // Q6: ctx cancel/deadline → close only
		}

		line := scanner.Bytes()
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		data, ok := bytes.CutPrefix(line, []byte("data: "))
		if !ok {
			continue
		}
		payload := strings.TrimSpace(string(data))
		if payload == "" {
			continue
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

		// usageMetadata is cumulative — last value wins. Capture it on
		// every chunk so the terminal emission has the latest totals.
		if chunk.UsageMetadata != nil {
			usage = provider.Usage{
				InputTokens:  chunk.UsageMetadata.PromptTokenCount,
				OutputTokens: chunk.UsageMetadata.CandidatesTokenCount,
			}
		}
		if len(chunk.Candidates) == 0 {
			continue
		}
		first := chunk.Candidates[0]
		if first.FinishReason != "" {
			finishReason = first.FinishReason
		}
		// Emit one delta per non-empty text part. Gemini sometimes
		// fragments a single delta into multiple parts within one
		// chunk — preserve the fragmentation rather than joining,
		// so TTFT measurement on the gateway side sees the earliest
		// possible signal.
		for _, part := range first.Content.Parts {
			if part.Text != "" {
				out <- provider.StreamChunk{ContentDelta: part.Text}
			}
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		out <- provider.StreamChunk{
			FinishReason: provider.FinishUnknown,
			Err:          mapTransportError(ctx, err),
		}
		return
	}

	// Normal EOF: emit terminal chunk with accumulated state.
	// TODO(v0.2): populate Raw with the last chunk's body so callers
	// can inspect Gemini-specific metadata (safetyRatings, etc.).
	// Deferred to keep PR-4 focused on the text path (ADR-007 Q3).
	out <- provider.StreamChunk{
		FinishReason: mapFinishReason(finishReason),
		Usage:        usage,
	}
}

// Compile-time assertion that *Client satisfies StreamingProvider.
var _ provider.StreamingProvider = (*Client)(nil)
