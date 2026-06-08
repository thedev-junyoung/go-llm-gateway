package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/metrics"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/router"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/tracing"
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

	// Tracing: open root span for the logical streaming operation.
	// The span is closed inside the producer goroutine (wrapStream)
	// when the channel closes — it lives for the full stream lifetime.
	spanCtx := g.tracing.OnChatStart(ctx, model)

	// streamStarted tracks whether we handed off to the producer goroutine.
	// If false when we return, the pre-stream failure path closes the span here.
	var lastErr error
	streamStarted := false
	defer func() {
		if !streamStarted {
			outcome := "error"
			if lastErr == nil {
				outcome = "no_streaming_provider"
			}
			g.tracing.OnChatEnd(spanCtx, outcome, lastErr)
		}
	}()

	primary, fallbacks, rerr := router.PickWithFallbacks(g.providers, req.Model)
	if rerr != nil {
		lastErr = rerr
		return nil, rerr
	}

	candidates := make([]provider.Provider, 0, 1+len(fallbacks))
	candidates = append(candidates, primary)
	candidates = append(candidates, fallbacks...)

	for i, p := range candidates {
		// Caller's ctx is the only time budget — abort early if cancelled
		// before we even try this candidate.
		if cerr := ctx.Err(); cerr != nil {
			if lastErr != nil {
				lastErr = fmt.Errorf("%w: last vendor error: %w", cerr, lastErr)
			} else {
				lastErr = cerr
			}
			return nil, lastErr
		}

		sp, ok := p.(provider.StreamingProvider)
		if !ok {
			// Streaming-incapable candidate. Skip without consuming a
			// failover slot — sync-only providers are silently ignored
			// here (they're still reachable via Chat).
			continue
		}

		// Tracing: child span for the pre-stream phase of this attempt.
		attemptCtx := g.tracing.OnAttemptStart(spanCtx, p.Name(), model, i)

		attemptStart := time.Now()
		stream, err := sp.ChatStream(attemptCtx, req)
		if err == nil {
			g.tracing.OnAttemptEnd(attemptCtx, "success", nil)
			streamStarted = true
			return wrapStream(spanCtx, g.metrics, g.tracing, p.Name(), model, attemptStart, stream), nil
		}

		g.tracing.OnAttemptEnd(attemptCtx, string(provider.OutcomeFromErr(err)), err)
		g.recordExemplar(attemptCtx, provider.AttemptInfo{
			Vendor:   p.Name(),
			Model:    model,
			AttemptN: i,
			Outcome:  provider.OutcomeFromErr(err),
			Error:    asProviderError(err),
		})

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
		lastErr = provider.NewProviderError(gatewayVendorLabel,
			provider.ErrorTypeInvalidInput, 0, false,
			fmt.Sprintf("no streaming provider supports model %q", req.Model), nil)
	}
	return nil, lastErr
}

// wrapStream intercepts the chunk stream to emit TTFT / stream_duration
// metrics (ADR-007) and tracing events (ADR-008). The wrapper adds one relay
// goroutine + a per-chunk channel hop; the per-chunk overhead is nanosecond
// scale for the channel forward, plus sub-µs OTel span event additions.
//
// spanCtx carries the root span opened by ChatStream; hook.OnChatEnd is
// called here (not in ChatStream) because the logical end of a streaming
// operation is when the channel closes, not when ChatStream returns.
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
func wrapStream(
	spanCtx context.Context,
	rec metrics.MetricRecorder,
	hook tracing.TracingHook,
	vendor, model string,
	attemptStart time.Time,
	in <-chan provider.StreamChunk,
) <-chan provider.StreamChunk {
	// ctx is the original caller context (for ctx.Err() cancellation checks);
	// spanCtx carries the root span and may differ when tracing is active.
	ctx := spanCtx
	out := make(chan provider.StreamChunk, cap(in))
	go func() {
		defer close(out)
		var (
			firstTokenSeen bool
			sawErrChunk    bool
			totalChunks    int
			midStreamErr   error
		)
		for ch := range in {
			if !firstTokenSeen && ch.ContentDelta != "" {
				firstTokenSeen = true
				hook.OnFirstToken(spanCtx)
				observeFirstTokenLatency(rec, vendor, model,
					metrics.StreamOutcomeSuccess, time.Since(attemptStart))
			}
			if ch.Err != nil {
				sawErrChunk = true
				midStreamErr = ch.Err
			}
			totalChunks++
			out <- ch
		}

		// Channel closed. Resolve outcomes for the not-yet-observed
		// histograms and tracing events.
		streamOutcome := metrics.StreamOutcomeSuccess
		failurePhase := ""
		switch {
		case sawErrChunk:
			streamOutcome = metrics.StreamOutcomeMidStreamError
			failurePhase = "mid_stream"
		case !firstTokenSeen && ctx.Err() != nil:
			streamOutcome = metrics.StreamOutcomeCtxCancelBeforeFirstChunk
			failurePhase = "ctx_cancel"
		case !firstTokenSeen:
			streamOutcome = metrics.StreamOutcomePreStreamFailure
			failurePhase = "mid_stream"
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

		hook.OnStreamEnd(spanCtx, streamOutcome, totalChunks, failurePhase)

		// Close root span — streaming logical end is channel close, not
		// ChatStream return (ADR-008 Q3).
		chatOutcome := "success"
		if streamOutcome != metrics.StreamOutcomeSuccess {
			chatOutcome = "error"
		}
		hook.OnChatEnd(spanCtx, chatOutcome, midStreamErr)
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
