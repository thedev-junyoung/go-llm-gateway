// Package oteltracing provides an OTel-backed implementation of
// tracing.TracingHook (ADR-008). It depends on go.opentelemetry.io/otel
// (API-only, ~300 KB) — callers who want tracing import this package and
// configure a TracerProvider + exporter in their main; callers who do not
// need tracing omit this import entirely (no OTel in their binary).
//
// Usage:
//
//	tp := /* set up your TracerProvider + exporter */
//	otel.SetTracerProvider(tp)
//
//	gw, _ := gateway.New(gateway.Config{
//	    Providers: [...],
//	    Tracing:   oteltracing.New(),
//	})
//
// If otel.SetTracerProvider has not been called the global NoopProvider is
// used — spans are created but immediately discarded. See godoc on New.
package oteltracing

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/tracing"
)

const tracerName = "go-llm-gateway"

// OtelTracingHook implements tracing.TracingHook and tracing.TraceIDExtractor
// using the OTel trace API. All methods are safe for concurrent use.
//
// IMPORTANT: call otel.SetTracerProvider before creating a Gateway with this
// hook. Without a real TracerProvider the global NoopProvider is active and
// no spans are exported.
//
// Streaming span lifetime: the root span opened by OnChatStart is closed by
// OnChatEnd, which the gateway calls from inside the ChatStream producer
// goroutine — the span may therefore remain open for O(seconds) to
// O(minutes). Configure your trace backend's open-span TTL accordingly.
type OtelTracingHook struct { //nolint:revive // OtelTracingHook follows project naming convention
	tracer trace.Tracer
}

// compile-time interface checks
var (
	_ tracing.TracingHook      = (*OtelTracingHook)(nil)
	_ tracing.TraceIDExtractor = (*OtelTracingHook)(nil)
)

// New returns an OtelTracingHook backed by the global OTel TracerProvider.
func New() *OtelTracingHook {
	return &OtelTracingHook{tracer: otel.Tracer(tracerName)}
}

// NewWithTracer returns an OtelTracingHook backed by the given tracer.
// Use for testing with an in-memory exporter.
func NewWithTracer(t trace.Tracer) *OtelTracingHook {
	return &OtelTracingHook{tracer: t}
}

// OnChatStart starts a root span named "llm_gateway.chat".
func (h *OtelTracingHook) OnChatStart(ctx context.Context, model string) context.Context {
	// span is stored in the returned ctx; _ discards the explicit span
	// reference because all subsequent access goes through
	// trace.SpanFromContext(ctx).
	ctx, _ = h.tracer.Start(ctx, "llm_gateway.chat",
		trace.WithAttributes(attribute.String("llm.model", model)),
	)
	return ctx
}

// OnChatEnd sets the final outcome on the root span and ends it.
func (h *OtelTracingHook) OnChatEnd(ctx context.Context, outcome string, err error) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("llm.outcome", outcome))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

// OnAttemptStart starts a child span named "llm_gateway.attempt".
func (h *OtelTracingHook) OnAttemptStart(ctx context.Context, vendor, model string, n int) context.Context {
	ctx, _ = h.tracer.Start(ctx, "llm_gateway.attempt",
		trace.WithAttributes(
			attribute.String("llm.vendor", vendor),
			attribute.String("llm.model", model),
			attribute.Int("llm.attempt_num", n),
		),
	)
	return ctx
}

// OnAttemptEnd sets the outcome on the attempt child span and ends it.
func (h *OtelTracingHook) OnAttemptEnd(ctx context.Context, outcome string, err error) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("llm.outcome", outcome))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

// OnFirstToken adds a "first_token" span event to the root span.
func (h *OtelTracingHook) OnFirstToken(ctx context.Context) {
	trace.SpanFromContext(ctx).AddEvent("first_token")
}

// OnStreamEnd adds a "stream_end" span event with outcome, total_chunks,
// and failure_phase (non-empty only on error outcomes).
func (h *OtelTracingHook) OnStreamEnd(ctx context.Context, outcome string, totalChunks int, failurePhase string) {
	attrs := []attribute.KeyValue{
		attribute.String("llm.outcome", outcome),
		attribute.Int("llm.total_chunks", totalChunks),
	}
	if failurePhase != "" {
		attrs = append(attrs, attribute.String("llm.failure_phase", failurePhase))
	}
	trace.SpanFromContext(ctx).AddEvent("stream_end", trace.WithAttributes(attrs...))
}

// ExtractTraceID implements tracing.TraceIDExtractor. Returns the 32-char
// hex OTel trace ID from ctx, or "" when no valid span is active (noop
// provider, otel.SetTracerProvider not called).
func (h *OtelTracingHook) ExtractTraceID(ctx context.Context) string {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
