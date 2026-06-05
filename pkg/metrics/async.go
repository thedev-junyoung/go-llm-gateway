package metrics

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// AsyncWrapper hands events off to a background goroutine so a slow inner
// recorder (OTel exporter, Datadog flush, etc.) does not add latency to the
// caller's Chat. Buffer is bounded — when full the wrapper drops the *new*
// event and bumps DroppedEvents so operators can alert on backpressure.
//
// CONTRACTS that callers MUST honor:
//
//  1. Close lifecycle. The background goroutine runs until Close. Call Close
//     once during graceful shutdown, AFTER all producers (the gateway) have
//     stopped — otherwise a producer race with Close can silently drop the
//     final N events. The wrapper does not promise drain completeness; the
//     ADR Risks table calls this out explicitly.
//
//  2. Context staleness. The ctx passed into OnAttempt is captured in the
//     channel and replayed by the consumer goroutine. By the time the inner
//     recorder sees it, the original request may have ended and the ctx
//     may be Done. Recording-only backends (Prometheus, log) are
//     context-agnostic and unaffected; trace-context-coupled backends like
//     OpenTelemetry's span propagation MUST NOT be wrapped here (parent
//     span would be lost). ADR-006 Q6 Sub-decision selected the godoc-ban
//     option over baking TraceID into AttemptInfo.
type AsyncWrapper struct {
	inner       MetricRecorder
	attemptCh   chan asyncAttempt
	failoverCh  chan asyncFailover
	dropCounter atomic.Uint64
	done        chan struct{}
	closeOnce   sync.Once
}

type asyncAttempt struct {
	ctx  context.Context
	info provider.AttemptInfo
}

type asyncFailover struct {
	ctx  context.Context
	info provider.FailoverInfo
}

// NewAsyncWrapper constructs the wrapper and spawns the consumer goroutine.
// bufSize sizes both internal channels independently — caller picks the
// trade-off between memory and how many events can queue before drops start.
// A sane starting point is 256 for typical RPS; raise it when DroppedEvents
// grows under load.
func NewAsyncWrapper(inner MetricRecorder, bufSize int) *AsyncWrapper {
	w := &AsyncWrapper{
		inner:      inner,
		attemptCh:  make(chan asyncAttempt, bufSize),
		failoverCh: make(chan asyncFailover, bufSize),
		done:       make(chan struct{}),
	}
	go w.consume()
	return w
}

// OnAttempt sends to the channel without blocking; on a full buffer the
// event is dropped and DroppedEvents increments.
func (w *AsyncWrapper) OnAttempt(ctx context.Context, info provider.AttemptInfo) {
	select {
	case w.attemptCh <- asyncAttempt{ctx: ctx, info: info}:
	default:
		w.dropCounter.Add(1)
	}
}

// OnFailover sends to the channel without blocking; on a full buffer the
// event is dropped and DroppedEvents increments.
func (w *AsyncWrapper) OnFailover(ctx context.Context, info provider.FailoverInfo) {
	select {
	case w.failoverCh <- asyncFailover{ctx: ctx, info: info}:
	default:
		w.dropCounter.Add(1)
	}
}

// DroppedEvents returns the cumulative count of events dropped because the
// internal buffer was full. Operators should expose this as a metric / alert
// — a non-zero value means the inner recorder is too slow for the load and
// observability has gaps.
func (w *AsyncWrapper) DroppedEvents() uint64 {
	return w.dropCounter.Load()
}

// ObserveFirstTokenLatency forwards SYNCHRONOUSLY to the inner recorder
// when it satisfies StreamingMetricRecorder; otherwise no-ops. Unlike
// OnAttempt/OnFailover which queue with drop-newest, streaming metric
// Observe is not buffered — histogram .Observe on Prometheus client_go
// is an atomic-ish O(1) bucket lookup, so the cost of async-ing it
// exceeds the cost of the sync call.
//
// If your inner StreamingMetricRecorder is a slow exporter (OTel
// remote push, network sink), wrap that recorder independently — not
// through AsyncWrapper — because the existing buffered design covers
// OnAttempt/OnFailover only.
func (w *AsyncWrapper) ObserveFirstTokenLatency(vendor, model, outcome string, d time.Duration) {
	if sr, ok := w.inner.(StreamingMetricRecorder); ok {
		sr.ObserveFirstTokenLatency(vendor, model, outcome, d)
	}
}

// ObserveStreamDuration forwards SYNCHRONOUSLY. See
// ObserveFirstTokenLatency for the sync-vs-async rationale.
func (w *AsyncWrapper) ObserveStreamDuration(vendor, model, outcome string, d time.Duration) {
	if sr, ok := w.inner.(StreamingMetricRecorder); ok {
		sr.ObserveStreamDuration(vendor, model, outcome, d)
	}
}

// Close stops the consumer goroutine. Best-effort drain of buffered events
// before returning; see the CONTRACT note above for the race window. Calling
// Close more than once is safe (no-op after the first).
func (w *AsyncWrapper) Close() error {
	w.closeOnce.Do(func() { close(w.done) })
	return nil
}

func (w *AsyncWrapper) consume() {
	for {
		select {
		case <-w.done:
			// Drain whatever is already in the channels at the moment we
			// observe done. Producers racing with Close past this point
			// are caller responsibility per the CONTRACT.
			for {
				select {
				case ev := <-w.attemptCh:
					w.inner.OnAttempt(ev.ctx, ev.info)
				case ev := <-w.failoverCh:
					w.inner.OnFailover(ev.ctx, ev.info)
				default:
					return
				}
			}
		case ev := <-w.attemptCh:
			w.inner.OnAttempt(ev.ctx, ev.info)
		case ev := <-w.failoverCh:
			w.inner.OnFailover(ev.ctx, ev.info)
		}
	}
}
