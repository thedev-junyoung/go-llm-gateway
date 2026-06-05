package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// MultiRecorder fans out events to several recorders. Use it when callers
// want both metrics and structured logs (or any other combination): wrap
// them with Multi(prom.New(), logrecorder.New()) and pass the result as
// gateway.Config.Metrics.
//
// Each underlying recorder is invoked inside its own recover(), so one
// recorder panicking does not silently skip the rest. The panic is logged
// via slog and the loop continues — partial observability is better than
// none. See ADR-006 round-5 Sub-decision.
type MultiRecorder []MetricRecorder

// Multi constructs a MultiRecorder from a variadic list — shorthand for
// MultiRecorder{a, b, c} that reads better at the call site.
func Multi(recorders ...MetricRecorder) MultiRecorder {
	return MultiRecorder(recorders)
}

// OnAttempt forwards info to every recorder, isolating panics.
func (m MultiRecorder) OnAttempt(ctx context.Context, info provider.AttemptInfo) {
	for _, r := range m {
		callAttempt(ctx, r, info)
	}
}

// OnFailover forwards info to every recorder, isolating panics.
func (m MultiRecorder) OnFailover(ctx context.Context, info provider.FailoverInfo) {
	for _, r := range m {
		callFailover(ctx, r, info)
	}
}

// callAttempt isolates one recorder's OnAttempt so a panic doesn't skip the
// rest of the slice. The recover() result is logged with the same field
// names the gateway uses elsewhere so operators can join across logs.
func callAttempt(ctx context.Context, r MetricRecorder, info provider.AttemptInfo) {
	defer func() {
		if p := recover(); p != nil {
			slog.ErrorContext(ctx, "metrics.MultiRecorder OnAttempt panicked",
				"panic", p,
				"vendor", info.Vendor,
				"model", info.Model,
			)
		}
	}()
	r.OnAttempt(ctx, info)
}

func callFailover(ctx context.Context, r MetricRecorder, info provider.FailoverInfo) {
	defer func() {
		if p := recover(); p != nil {
			slog.ErrorContext(ctx, "metrics.MultiRecorder OnFailover panicked",
				"panic", p,
				"from_vendor", info.FromVendor,
				"to_vendor", info.ToVendor,
			)
		}
	}()
	r.OnFailover(ctx, info)
}

// ObserveFirstTokenLatency forwards to every recorder that satisfies
// StreamingMetricRecorder. Sync-only recorders in the slice are
// silently skipped — same idiom the gateway uses to filter streaming
// providers. Per-recorder recover() isolates panics.
func (m MultiRecorder) ObserveFirstTokenLatency(vendor, model, outcome string, d time.Duration) {
	for _, r := range m {
		sr, ok := r.(StreamingMetricRecorder)
		if !ok {
			continue
		}
		callObserveTTFT(sr, vendor, model, outcome, d)
	}
}

// ObserveStreamDuration forwards to every streaming-aware recorder.
func (m MultiRecorder) ObserveStreamDuration(vendor, model, outcome string, d time.Duration) {
	for _, r := range m {
		sr, ok := r.(StreamingMetricRecorder)
		if !ok {
			continue
		}
		callObserveStreamDuration(sr, vendor, model, outcome, d)
	}
}

func callObserveTTFT(r StreamingMetricRecorder, vendor, model, outcome string, d time.Duration) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("metrics.MultiRecorder ObserveFirstTokenLatency panicked",
				"panic", p, "vendor", vendor, "model", model, "outcome", outcome)
		}
	}()
	r.ObserveFirstTokenLatency(vendor, model, outcome, d)
}

func callObserveStreamDuration(r StreamingMetricRecorder, vendor, model, outcome string, d time.Duration) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("metrics.MultiRecorder ObserveStreamDuration panicked",
				"panic", p, "vendor", vendor, "model", model, "outcome", outcome)
		}
	}()
	r.ObserveStreamDuration(vendor, model, outcome, d)
}
