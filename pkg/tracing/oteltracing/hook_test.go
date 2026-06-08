package oteltracing_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/tracing"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/tracing/oteltracing"
)

// newTestHook returns an OtelTracingHook wired to an in-memory span exporter
// so tests can assert on emitted spans without a real backend.
func newTestHook() (*oteltracing.OtelTracingHook, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
	return oteltracing.NewWithTracer(tp.Tracer("test")), sr
}

func TestOtelTracingHook_ImplementsInterfaces(t *testing.T) {
	var _ tracing.TracingHook = oteltracing.New()
	var _ tracing.TraceIDExtractor = oteltracing.New()
}

func TestOtelTracingHook_ChatSpan(t *testing.T) {
	hook, sr := newTestHook()
	ctx := context.Background()

	ctx = hook.OnChatStart(ctx, "gpt-4o")
	hook.OnChatEnd(ctx, "success", nil)

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 ended span, got %d", len(spans))
	}
	if spans[0].Name() != "llm_gateway.chat" {
		t.Errorf("span name = %q, want llm_gateway.chat", spans[0].Name())
	}
}

func TestOtelTracingHook_AttemptChildSpan(t *testing.T) {
	hook, sr := newTestHook()
	ctx := context.Background()

	ctx = hook.OnChatStart(ctx, "gpt-4o")
	attemptCtx := hook.OnAttemptStart(ctx, "openai", "gpt-4o", 0)
	hook.OnAttemptEnd(attemptCtx, "success", nil)
	hook.OnChatEnd(ctx, "success", nil)

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("expected 2 ended spans (attempt + chat), got %d", len(spans))
	}
	// attempt span ends first
	if spans[0].Name() != "llm_gateway.attempt" {
		t.Errorf("first ended span = %q, want llm_gateway.attempt", spans[0].Name())
	}
	// attempt span must be a child of the chat span
	if spans[0].Parent().SpanID() != spans[1].SpanContext().SpanID() {
		t.Error("attempt span parent should be the chat span")
	}
}

func TestOtelTracingHook_ExtractTraceID(t *testing.T) {
	hook, _ := newTestHook()
	ctx := context.Background()

	// No span yet → empty string.
	if id := hook.ExtractTraceID(ctx); id != "" {
		t.Errorf("expected empty trace ID before span, got %q", id)
	}

	ctx = hook.OnChatStart(ctx, "gpt-4o")
	id := hook.ExtractTraceID(ctx)
	if len(id) != 32 {
		t.Errorf("expected 32-char trace ID, got %q (len=%d)", id, len(id))
	}
	hook.OnChatEnd(ctx, "success", nil)
}

func TestOtelTracingHook_StreamEvents(t *testing.T) {
	hook, sr := newTestHook()
	ctx := context.Background()

	ctx = hook.OnChatStart(ctx, "gpt-4o")
	hook.OnFirstToken(ctx)
	hook.OnStreamEnd(ctx, "success", 10, "")
	hook.OnChatEnd(ctx, "success", nil)

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	events := spans[0].Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events (first_token + stream_end), got %d", len(events))
	}
	if events[0].Name != "first_token" {
		t.Errorf("event[0] = %q, want first_token", events[0].Name)
	}
	if events[1].Name != "stream_end" {
		t.Errorf("event[1] = %q, want stream_end", events[1].Name)
	}
}
