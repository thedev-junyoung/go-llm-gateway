package metrics

import (
	"context"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// ExemplarRecorder is an optional extension of MetricRecorder for backends
// that support Prometheus exemplars (ADR-008 Q6). Gateway type-asserts; if
// the configured recorder does not implement this, exemplar attachment is
// silently skipped.
//
// The gateway calls ObserveAttemptWithExemplar immediately after
// OnAttemptEnd, on the same goroutine, with the traceID extracted from the
// attempt's child span context. This design is stateless — no shared mutable
// state between tracing and metrics (see ADR-008 Q6, Alt 5 rejection of the
// push-callback / ExemplarTarget pattern).
//
// traceID is a 32-char hex OTel TraceID string, or "" to disable the
// exemplar attachment for this observation.
type ExemplarRecorder interface {
	MetricRecorder
	ObserveAttemptWithExemplar(ctx context.Context, info provider.AttemptInfo, traceID string)
}
