package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/metrics"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/router"
)

// ChatStream is the streaming counterpart to Chat. It routes the request
// through the same primary+fallbacks chain the router returns, but emits
// each candidate's response incrementally via the returned channel.
//
// ADR-007 contract summary:
//
//   - Pre-stream failover (Q4): any retriable error returned from a
//     provider's ChatStream before any chunk reaches the caller falls
//     over to the next candidate. Once the first chunk has been
//     produced, mid-stream errors terminate the stream — they do NOT
//     trigger a vendor switch (Q4 — typewriter UX consistency over
//     protocol convenience).
//
//   - Streaming-capable filter: only candidates that implement
//     StreamingProvider participate. Sync-only providers in the
//     Config.Providers slice are silently skipped here (they're still
//     reachable via the synchronous Chat method). If no candidate is
//     streaming-capable, ChatStream synthesizes an InvalidInput
//     *ProviderError with Vendor="gateway" — the same shape the router
//     uses for "no provider supports this model".
//
//   - Pre-stream errors return (nil, *ProviderError) without
//     allocating the channel. Mid-stream errors surface as a terminal
//     StreamChunk with Err set, followed by channel close (Q7).
//
// CONTRACT: caller MUST `defer cancel()` on the ctx passed to
// ChatStream. The producer goroutine watches ctx.Done(); leaving the
// channel range loop without cancelling the ctx leaves the producer
// parked on a send until the underlying HTTP transport eventually
// returns. See ADR-007 Risks table.
//
// TTFT and stream_duration metrics: when Config.Metrics satisfies
// metrics.StreamingMetricRecorder, the returned channel is wrapped
// with a relay goroutine that emits one TTFT observation per
// candidate (success on first ContentDelta, pre_stream_failure on
// vendor reject, ctx_cancel_before_first_chunk on caller abort) and
// one stream_duration observation at channel close (success /
// mid_stream_error / ctx_cancel_before_first_chunk). Recorders that
// don't satisfy the streaming interface are silently bypassed.
func (g *Gateway) ChatStream(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	model := normalizeModel(req.Model, g.knownModels)

	primary, fallbacks, rerr := router.PickWithFallbacks(g.providers, req.Model)
	if rerr != nil {
		// Router failure is a pre-stream condition; surface it the same
		// way Chat does so callers see a consistent error shape across
		// sync and streaming paths.
		return nil, rerr
	}

	candidates := make([]provider.Provider, 0, 1+len(fallbacks))
	candidates = append(candidates, primary)
	candidates = append(candidates, fallbacks...)

	var lastErr error
	for _, p := range candidates {
		// Caller's ctx is the only time budget — abort early if cancelled
		// before we even try this candidate.
		if cerr := ctx.Err(); cerr != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("%w: last vendor error: %w", cerr, lastErr)
			}
			return nil, cerr
		}

		sp, ok := p.(provider.StreamingProvider)
		if !ok {
			// Streaming-incapable candidate. Skip without consuming a
			// failover slot — sync-only providers are silently ignored
			// here (they're still reachable via Chat).
			continue
		}

		attemptStart := time.Now()
		stream, err := sp.ChatStream(ctx, req)
		if err == nil {
			return wrapStreamWithMetrics(ctx, g.metrics, p.Name(), model, attemptStart, stream), nil
		}

		// Pre-stream failure path: ADR-007 Q5 outcome enum reserves
		// pre_stream_failure for exactly this. Emit so the histogram
		// labels stay accountable for every ChatStream call, not just
		// the ones that reached a chunk.
		observeFirstTokenLatency(g.metrics, p.Name(), model,
			metrics.StreamOutcomePreStreamFailure, time.Since(attemptStart))

		lastErr = err
		if !shouldFailover(err) {
			return nil, err
		}
		// retriable — try the next streaming candidate.
	}

	// Guard against returning a nil channel — caller's range loop on
	// nil deadlocks. If every candidate was streaming-incapable and no
	// vendor-level error was recorded, synthesize a "no streaming
	// provider supports this model" *ProviderError matching the
	// router's "no provider supports model" shape.
	if lastErr == nil {
		return nil, provider.NewProviderError(gatewayVendorLabel,
			provider.ErrorTypeInvalidInput, 0, false,
			fmt.Sprintf("no streaming provider supports model %q", req.Model), nil)
	}
	return nil, lastErr
}

// wrapStreamWithMetrics intercepts the chunk stream to emit TTFT and
// stream_duration observations. Implements the wrapWithMetrics sketch
// from ADR-007 Synthesis. The wrapper adds one relay goroutine + a
// per-chunk channel hop; benchmarks pin the per-chunk overhead at
// nanosecond scale.
//
// Outcome label resolution:
//
//   - Success path: first ContentDelta!="" → TTFT=success. Channel
//     close with no Err chunk → stream_duration=success.
//   - First-token never reached + ctx.Err() != nil at close →
//     TTFT=ctx_cancel_before_first_chunk. stream_duration uses the
//     same outcome.
//   - First-token never reached + ctx still live (adapter closed
//     without delta + Err) → TTFT=pre_stream_failure even though we
//     got past sp.ChatStream — covers the "200 with empty stream"
//     vendor quirk where no error surfaces but no content arrives.
//   - Mid-stream Err chunk → TTFT already observed Success if any
//     content reached us; stream_duration=mid_stream_error.
func wrapStreamWithMetrics(ctx context.Context, rec metrics.MetricRecorder,
	vendor, model string, attemptStart time.Time,
	in <-chan provider.StreamChunk,
) <-chan provider.StreamChunk {
	out := make(chan provider.StreamChunk, cap(in))
	go func() {
		defer close(out)
		var (
			firstTokenSeen bool
			sawErrChunk    bool
		)
		for ch := range in {
			if !firstTokenSeen && ch.ContentDelta != "" {
				firstTokenSeen = true
				observeFirstTokenLatency(rec, vendor, model,
					metrics.StreamOutcomeSuccess, time.Since(attemptStart))
			}
			if ch.Err != nil {
				sawErrChunk = true
			}
			out <- ch
		}

		// Channel closed. Resolve outcomes for the not-yet-observed
		// histograms.
		streamOutcome := metrics.StreamOutcomeSuccess
		switch {
		case sawErrChunk:
			streamOutcome = metrics.StreamOutcomeMidStreamError
		case !firstTokenSeen && ctx.Err() != nil:
			streamOutcome = metrics.StreamOutcomeCtxCancelBeforeFirstChunk
		case !firstTokenSeen:
			streamOutcome = metrics.StreamOutcomePreStreamFailure
		}
		if !firstTokenSeen {
			ttftOutcome := metrics.StreamOutcomePreStreamFailure
			if ctx.Err() != nil {
				ttftOutcome = metrics.StreamOutcomeCtxCancelBeforeFirstChunk
			}
			observeFirstTokenLatency(rec, vendor, model, ttftOutcome,
				time.Since(attemptStart))
		}
		observeStreamDuration(rec, vendor, model, streamOutcome,
			time.Since(attemptStart))
	}()
	return out
}

// observeFirstTokenLatency / observeStreamDuration thin helpers that
// type-assert the gateway's MetricRecorder into a
// StreamingMetricRecorder before forwarding. Sync-only recorders
// (NoOp, custom Provider-only implementations) silently skip.
func observeFirstTokenLatency(rec metrics.MetricRecorder, vendor, model, outcome string, d time.Duration) {
	if sr, ok := rec.(metrics.StreamingMetricRecorder); ok {
		sr.ObserveFirstTokenLatency(vendor, model, outcome, d)
	}
}

func observeStreamDuration(rec metrics.MetricRecorder, vendor, model, outcome string, d time.Duration) {
	if sr, ok := rec.(metrics.StreamingMetricRecorder); ok {
		sr.ObserveStreamDuration(vendor, model, outcome, d)
	}
}
