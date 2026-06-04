package provider

import (
	"context"
	"encoding/json"
)

// StreamingProvider extends Provider with a streaming Chat surface.
// Adapters that support server-sent-event style incremental generation
// (OpenAI / Anthropic / Gemini in v0.2) implement this; adapters that
// only support synchronous Chat satisfy Provider alone. Callers branch
// via type assertion — see ADR-007 Q2 for why this is a separate
// interface instead of an additional method on Provider.
//
// The returned channel is owned by the adapter. The adapter MUST close
// the channel when:
//
//   - the stream completes normally (terminal chunk with FinishReason);
//   - the caller's ctx is cancelled (no terminal chunk needed — channel
//     close + ctx.Err() is the signal, per ADR-007 Q6);
//   - a mid-stream error occurs (emit one StreamChunk with Err set, then
//     close — see ADR-007 Q7).
//
// Pre-stream failures (HTTP 4xx/5xx before the first chunk, transport
// errors, model not supported) return (nil, *ProviderError) without
// channel allocation. The gateway's failover loop only honors these
// pre-stream errors — once any chunk has been delivered to the caller,
// mid-stream errors terminate the stream rather than triggering a
// silent vendor switch (ADR-007 Q4).
type StreamingProvider interface {
	Provider
	ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error)
}

// StreamChunk is one delta emitted by a StreamingProvider. The wire
// format of each vendor differs (OpenAI delta object, Anthropic
// content_block_delta event, Gemini JSON line), but every adapter
// normalizes to this shape so the gateway and caller see a single
// vocabulary. See ADR-007 Q3 for the delta-vs-cumulative choice.
type StreamChunk struct {
	// ContentDelta is the incremental text produced by this chunk.
	// Concatenating ContentDelta across every non-error chunk yields
	// the same string a synchronous Chat would have returned. Empty on
	// metadata-only chunks (some vendors emit a role-only first event;
	// the terminal chunk that carries Usage/FinishReason is also
	// typically empty).
	ContentDelta string

	// FinishReason is non-empty ONLY on the terminal chunk. Callers
	// should detect normal end-of-stream via this field, not channel
	// close — channel close also covers the ctx-cancelled case where
	// no terminal chunk was emitted (ADR-007 Q6).
	FinishReason FinishReason

	// Usage is populated ONLY on the terminal chunk. OpenAI requires
	// stream_options.include_usage=true (the adapter sets this
	// automatically); Anthropic and Gemini emit it inline. Reconcile
	// with RateLimit.Record once the stream completes.
	Usage Usage

	// Raw is the original vendor event JSON (one parsed event, not
	// the whole stream). The v0.2 escape hatch for tool_use deltas,
	// vendor safety signals, and multi-modal blocks — same role as
	// ChatResponse.Raw in the synchronous path.
	Raw json.RawMessage

	// Err is non-nil ONLY on the terminal chunk when the stream failed
	// mid-flight (vendor 5xx event mid-stream, JSON parse error on an
	// event, transport closed unexpectedly). When Err is non-nil,
	// ContentDelta is empty and FinishReason is FinishUnknown. The
	// channel closes immediately after this chunk, so callers MUST
	// check Err inside the range loop — there is no separate error
	// channel (ADR-007 Q7). Err is *ProviderError when the cause is
	// classifiable; errors.As + sentinel patterns work the same as on
	// the synchronous Chat error path.
	Err error
}
