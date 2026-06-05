package metrics

import (
	"context"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// MetricRecorder receives per-attempt and per-failover events from the
// gateway. See the package doc for the latency CONTRACT.
type MetricRecorder interface {
	OnAttempt(ctx context.Context, info provider.AttemptInfo)
	OnFailover(ctx context.Context, info provider.FailoverInfo)
}

// NoOpRecorder discards every event. It is the default the gateway installs
// when Config.Metrics is nil — having a non-nil default lets the gateway
// invoke the hooks unconditionally without per-call nil checks. Callers can
// also use it explicitly to disable recording without changing wiring.
type NoOpRecorder struct{}

// OnAttempt does nothing.
func (NoOpRecorder) OnAttempt(context.Context, provider.AttemptInfo) {}

// OnFailover does nothing.
func (NoOpRecorder) OnFailover(context.Context, provider.FailoverInfo) {}

// ObserveFirstTokenLatency does nothing. Implemented so NoOpRecorder
// also satisfies StreamingMetricRecorder — the gateway's
// wrapWithMetrics path can type-assert without a nil branch.
func (NoOpRecorder) ObserveFirstTokenLatency(string, string, string, time.Duration) {}

// ObserveStreamDuration does nothing. See ObserveFirstTokenLatency.
func (NoOpRecorder) ObserveStreamDuration(string, string, string, time.Duration) {}

// Compile-time interface checks. Pinning every public recorder type here
// surfaces signature drift at build time instead of at the first OnAttempt
// call from a real caller.
var (
	_ MetricRecorder          = NoOpRecorder{}
	_ MetricRecorder          = MultiRecorder(nil)
	_ MetricRecorder          = (*AsyncWrapper)(nil)
	_ StreamingMetricRecorder = NoOpRecorder{}
	_ StreamingMetricRecorder = MultiRecorder(nil)
	_ StreamingMetricRecorder = (*AsyncWrapper)(nil)
)
