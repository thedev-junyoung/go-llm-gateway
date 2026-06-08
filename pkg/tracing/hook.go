// Package tracing defines the TracingHook interface that gateway wires
// into Chat and ChatStream for distributed tracing (ADR-008).
//
// The core package has zero external dependencies — TracingHook and
// NoOpTracingHook use only stdlib. Callers who want OTel spans import
// the sub-package pkg/tracing/oteltracing, which depends on
// go.opentelemetry.io/otel (API-only, ~300 KB).
//
// CONTRACT: all TracingHook methods are called synchronously on the
// caller's goroutine. Implementations MUST NOT block. OTel span
// operations are sub-µs in-memory; slower backends must buffer
// internally — do NOT wrap TracingHook in metrics.AsyncWrapper (that
// wrapper's ctx-forwarding breaks span extraction, see ADR-008 Q5).
package tracing

import "context"

// TracingHook receives lifecycle events from gateway.Chat and
// gateway.ChatStream. The gateway calls the hook at fixed points:
//
//	OnChatStart → [0..N] OnAttemptStart/OnAttemptEnd pairs → OnChatEnd
//
// For ChatStream, OnChatEnd is called from inside the producer goroutine
// when the chunk channel closes — the root span therefore lives for the
// full stream lifetime, not just until ChatStream returns.
//
// OnFirstToken and OnStreamEnd are ChatStream-only; they are no-ops on
// the sync Chat path.
type TracingHook interface { //nolint:revive // TracingHook follows project naming convention (cf. metrics.MetricRecorder)
	// OnChatStart is called at the entry of gateway.Chat or
	// gateway.ChatStream, before the router runs. Returns a context
	// carrying the root span; all subsequent hook calls receive this ctx.
	OnChatStart(ctx context.Context, model string) context.Context

	// OnChatEnd closes the root span. outcome is "success" or "error".
	// For ChatStream it is called from the producer goroutine on channel
	// close, not when ChatStream returns.
	OnChatEnd(ctx context.Context, outcome string, err error)

	// OnAttemptStart creates a child span for one provider attempt
	// (pre-stream phase for ChatStream). Returns a context carrying the
	// child span; the caller must pass this ctx to OnAttemptEnd and to
	// the provider call so OTel propagates into the HTTP request.
	OnAttemptStart(ctx context.Context, vendor, model string, attemptNum int) context.Context

	// OnAttemptEnd closes the attempt child span. ctx must be the value
	// returned by the matching OnAttemptStart — NOT the root span ctx.
	OnAttemptEnd(ctx context.Context, outcome string, err error)

	// OnFirstToken is called when the first ContentDelta!="" chunk
	// arrives from ChatStream. Recorded as a span event on the root span.
	OnFirstToken(ctx context.Context)

	// OnStreamEnd is called when the ChatStream producer goroutine closes
	// the channel. failurePhase is non-empty only on error outcomes:
	// "mid_stream" (Err chunk received), "ctx_cancel" (caller cancelled),
	// "pre_stream" (stream closed with no content and no error chunk);
	// empty string on normal success.
	OnStreamEnd(ctx context.Context, outcome string, totalChunks int, failurePhase string)
}

// TraceIDExtractor is an optional interface for TracingHook implementations
// that can surface the active OTel trace ID from a context. Gateway
// type-asserts; if the hook does not implement this, exemplar attachment
// is silently skipped (ADR-008 Q6).
type TraceIDExtractor interface {
	// ExtractTraceID returns the 32-char hex OTel trace ID from ctx, or
	// "" if no valid span is present (noop provider, unset TracerProvider).
	ExtractTraceID(ctx context.Context) string
}

// NoOpTracingHook discards all events and returns ctx unchanged. It is the
// gateway default when Config.Tracing is nil — callers that don't need
// tracing pay zero overhead beyond a nil check in New.
type NoOpTracingHook struct{}

// OnChatStart returns ctx unchanged.
func (NoOpTracingHook) OnChatStart(ctx context.Context, _ string) context.Context { return ctx }

// OnChatEnd does nothing.
func (NoOpTracingHook) OnChatEnd(context.Context, string, error) {}

// OnAttemptStart returns ctx unchanged.
func (NoOpTracingHook) OnAttemptStart(ctx context.Context, _, _ string, _ int) context.Context {
	return ctx
}

// OnAttemptEnd does nothing.
func (NoOpTracingHook) OnAttemptEnd(context.Context, string, error) {}

// OnFirstToken does nothing.
func (NoOpTracingHook) OnFirstToken(context.Context) {}

// OnStreamEnd does nothing.
func (NoOpTracingHook) OnStreamEnd(context.Context, string, int, string) {}

// compile-time interface check
var _ TracingHook = NoOpTracingHook{}
