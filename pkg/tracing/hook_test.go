package tracing_test

import (
	"context"
	"testing"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/tracing"
)

func TestNoOpTracingHook_ReturnsCtxUnchanged(t *testing.T) {
	hook := tracing.NoOpTracingHook{}
	ctx := context.Background()

	if got := hook.OnChatStart(ctx, "gpt-4o"); got != ctx {
		t.Error("OnChatStart must return the same ctx for NoOp")
	}
	if got := hook.OnAttemptStart(ctx, "openai", "gpt-4o", 0); got != ctx {
		t.Error("OnAttemptStart must return the same ctx for NoOp")
	}
}

func TestNoOpTracingHook_DoesNotPanic(_ *testing.T) {
	hook := tracing.NoOpTracingHook{}
	ctx := context.Background()

	hook.OnChatEnd(ctx, "success", nil)
	hook.OnAttemptEnd(ctx, "success", nil)
	hook.OnFirstToken(ctx)
	hook.OnStreamEnd(ctx, "success", 42, "")
}

// Verify TraceIDExtractor is a distinct optional interface.
func TestTraceIDExtractor_NoOpDoesNotImplement(t *testing.T) {
	var hook tracing.TracingHook = tracing.NoOpTracingHook{}
	if _, ok := hook.(tracing.TraceIDExtractor); ok {
		t.Error("NoOpTracingHook should not implement TraceIDExtractor")
	}
}
