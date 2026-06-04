package gateway

import (
	"context"
	"fmt"

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
// TTFT measurement and stream_duration metrics are wired in a follow-
// up PR (PR-6 in the ADR-007 series). v0.2 callers see the stream
// directly from the chosen adapter; the metric wrapper goroutine
// will be slotted between adapter and caller without breaking this
// signature.
func (g *Gateway) ChatStream(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamChunk, error) {
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

		stream, err := sp.ChatStream(ctx, req)
		if err == nil {
			return stream, nil
		}
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
