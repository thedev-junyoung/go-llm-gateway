package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/metrics"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/ratelimit"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/router"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/types"
)

// gatewayVendorLabel is the synthetic Vendor value used on AttemptInfo when
// the router itself refused the request (no provider supports the model).
// Pinned so dashboards can branch "vendor=gateway" → routing failures vs
// real vendor surfaces. ADR-006 Q3.
const gatewayVendorLabel = "gateway"

// Config configures a Gateway. Only Providers is required in v0.1.
//
// Failover trigger conditions are fixed by ADR-004 (every Retriable=true
// *ProviderError triggers a switch to the next supporting provider, no
// in-provider retry, no inter-attempt backoff, caller's ctx is the only
// time budget).
type Config struct {
	// Providers is the ordered set of vendor adapters. The slice order IS the
	// priority — for a given model, the first provider whose SupportsModel
	// returns true serves the request (see ADR-003).
	Providers []provider.Provider

	// RateLimit, when non-nil, is consulted before every provider attempt
	// and reconciled with actual Usage after every successful call (ADR-005).
	// A denied Allow surfaces as a *provider.ProviderError with Type ==
	// ErrorTypeRateLimit and triggers the same failover loop as a vendor 429
	// (ADR-005 Q7). Backend errors are logged and the request proceeds —
	// FailOpen is the contract default; the vendor's own 429 remains the
	// safety net.
	RateLimit ratelimit.RateLimiter

	// Metrics, when non-nil, receives one OnAttempt per Chat attempt
	// (vendor / gateway-router / gateway-preempt) and one OnFailover per
	// router handoff between consecutive retriable failures. Nil installs
	// metrics.NoOpRecorder so the Chat loop can invoke hooks unconditionally
	// without per-call nil checks.
	//
	// The hooks run synchronously on the caller's goroutine. Slow backends
	// (OTel exporter, Datadog flush) MUST be wrapped in metrics.AsyncWrapper
	// — see pkg/metrics doc CONTRACT.
	Metrics metrics.MetricRecorder

	// KnownModels caps the Model label cardinality on emitted AttemptInfo:
	// req.Model values absent from this slice are reported as "unknown" on
	// both MetricRecorder.OnAttempt and ChatResponse.Attempts.
	//
	// A nil/empty slice is the strict default — EVERY model collapses to
	// "unknown" until operators register their models here. The rationale
	// (ADR-006 Q7) is cardinality protection plus an explicit alert signal:
	// a spike in unknown_model_total means a new vendor model id leaked
	// through without configuration. Adopt early, register everything you
	// care about; don't rely on the default for log-only setups.
	KnownModels []string
}

// Gateway is the composition root: it owns the providers and dispatches
// Chat through the routing layer. Construct with New, never via struct
// literal — future ADRs may add invariants (initialized rate limiter,
// metrics recorder, etc.) that the constructor enforces.
type Gateway struct {
	providers   []provider.Provider
	rateLimit   ratelimit.RateLimiter  // nil means rate limiting disabled
	metrics     metrics.MetricRecorder // never nil after New; defaults to NoOp
	knownModels map[string]struct{}    // empty map normalizes all to "unknown" (ADR-006 Q7)
}

// ErrNoProviders is returned by New when Config.Providers is empty.
var ErrNoProviders = errors.New("gateway: Config.Providers must contain at least one provider")

// New validates the config and returns a ready Gateway. Rejects an empty
// Providers slice and rejects nil entries within it — both would surface
// later as nil-deref panics inside router.Pick / Provider.Chat.
func New(cfg Config) (*Gateway, error) {
	if len(cfg.Providers) == 0 {
		return nil, ErrNoProviders
	}
	for i, p := range cfg.Providers {
		if p == nil {
			return nil, fmt.Errorf("gateway: Config.Providers[%d] is nil", i)
		}
	}

	rec := cfg.Metrics
	if rec == nil {
		rec = metrics.NoOpRecorder{}
	}

	known := make(map[string]struct{}, len(cfg.KnownModels))
	for _, m := range cfg.KnownModels {
		known[m] = struct{}{}
	}

	return &Gateway{
		providers:   cfg.Providers,
		rateLimit:   cfg.RateLimit,
		metrics:     rec,
		knownModels: known,
	}, nil
}

// Chat routes the request through the candidate chain returned by the
// router and applies the ADR-004 failover policy:
//
//   - any *ProviderError with Retriable==true falls over to the next
//     supporting provider in config order;
//   - non-retriable errors abort immediately (no fallback attempt);
//   - the caller's ctx is the only time budget — Chat checks ctx.Err()
//     between attempts and surfaces cancellation as a dual-wrapped error
//     so errors.Is works for both context.Canceled / DeadlineExceeded
//     AND the most recent vendor sentinel.
//
// Observability (ADR-006): every attempt is emitted to Config.Metrics with
// one of three Origin labels — vendor / gateway-preempt / gateway-router.
// On success, ChatResponse.Attempts mirrors the same records so callers can
// correlate by request_id without subscribing to the recorder. On error
// paths Attempts stays empty — the recorder is the canonical sink there.
func (g *Gateway) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	model := normalizeModel(req.Model, g.knownModels)

	primary, fallbacks, rerr := router.PickWithFallbacks(g.providers, req.Model)
	if rerr != nil {
		// Router failure: emit one synthetic attempt so the failure shows up
		// on dashboards even though no provider was ever asked. Vendor label
		// is "gateway" so operators can branch on Origin == gateway-router.
		recordAttempt(ctx, g.metrics, provider.AttemptInfo{
			Vendor:  gatewayVendorLabel,
			Model:   model,
			Outcome: provider.OutcomeFromErr(rerr),
			Origin:  types.OriginGatewayRouter,
			Error:   asProviderError(rerr),
		})
		return provider.ChatResponse{}, rerr
	}

	candidates := make([]provider.Provider, 0, 1+len(fallbacks))
	candidates = append(candidates, primary)
	candidates = append(candidates, fallbacks...)

	attempts := make([]provider.AttemptInfo, 0, len(candidates))
	var lastErr error
	for i, p := range candidates {
		// Caller's context wins — abort before the next attempt if cancelled.
		// Dual-wrap preserves both errors so callers can:
		//   errors.Is(err, context.Canceled)         // detect cancellation
		//   errors.Is(err, provider.ErrRateLimited)  // see why we stopped
		if cerr := ctx.Err(); cerr != nil {
			if lastErr != nil {
				return provider.ChatResponse{}, fmt.Errorf("%w: last vendor error: %w", cerr, lastErr)
			}
			return provider.ChatResponse{}, cerr
		}

		// Rate-limit pre-check (ADR-005). A denied Allow produces the same
		// sentinel as a vendor 429 so the failover loop walks to the next
		// candidate without special-casing the origin (ADR-005 Q7).
		if g.rateLimit != nil {
			d, lerr := g.rateLimit.Allow(ctx, p.Name(), p.KeyHash(), req)
			if lerr != nil {
				// FailOpen: log and proceed.
				slog.WarnContext(ctx, "ratelimit backend error",
					"vendor", p.Name(), "err", lerr)
			} else if !d.Allow {
				pe := provider.NewProviderError(p.Name(), provider.ErrorTypeRateLimit, 0, true,
					"rate limited by gateway", nil)
				if d.RetryAfter != nil {
					pe = pe.WithRetryAfter(*d.RetryAfter)
				}

				info := provider.AttemptInfo{
					Vendor:   p.Name(),
					Model:    model,
					AttemptN: i,
					Outcome:  types.OutcomeErrorRateLimit,
					Origin:   types.OriginGatewayPreempt,
					Error:    pe,
				}
				attempts = append(attempts, info)
				recordAttempt(ctx, g.metrics, info)

				lastErr = pe
				g.emitFailoverIfMore(ctx, candidates, i, pe.Type)
				continue
			}
		}

		start := time.Now()
		resp, cerr := p.Chat(ctx, req)
		duration := time.Since(start)

		info := provider.AttemptInfo{
			Vendor:   p.Name(),
			Model:    model,
			AttemptN: i,
			Outcome:  provider.OutcomeFromErr(cerr),
			Origin:   types.OriginVendor,
			Duration: duration,
			Usage:    resp.Usage, // zero on error paths — adapters return zero-Usage there
			Error:    asProviderError(cerr),
		}
		attempts = append(attempts, info)
		recordAttempt(ctx, g.metrics, info)

		if cerr == nil {
			if g.rateLimit != nil {
				if rrerr := g.rateLimit.Record(ctx, p.Name(), p.KeyHash(), resp.Usage); rrerr != nil {
					slog.WarnContext(ctx, "ratelimit record failed",
						"vendor", p.Name(), "err", rrerr)
				}
			}
			resp.Attempts = attempts
			return resp, nil
		}
		lastErr = cerr

		if !shouldFailover(cerr) {
			return provider.ChatResponse{}, cerr
		}
		// shouldFailover already verified *ProviderError; safe to extract.
		if pe := asProviderError(cerr); pe != nil {
			g.emitFailoverIfMore(ctx, candidates, i, pe.Type)
		}
	}

	// Exhausted every supporting provider with retriable failures.
	return provider.ChatResponse{}, lastErr
}

// emitFailoverIfMore records a FailoverInfo only when a next candidate
// exists. Centralised because both the pre-empt path and the vendor-error
// path need the same guard; missing the bounds check on either side would
// either over-report (synthesizing a failover to nowhere) or under-report
// (skipping a real handoff).
func (g *Gateway) emitFailoverIfMore(ctx context.Context, candidates []provider.Provider, current int, reason provider.ErrorType) {
	if current+1 >= len(candidates) {
		return
	}
	recordFailover(ctx, g.metrics, provider.FailoverInfo{
		FromVendor: candidates[current].Name(),
		ToVendor:   candidates[current+1].Name(),
		Reason:     reason,
	})
}

// shouldFailover reports whether err is a *provider.ProviderError marked
// Retriable. Unknown error types (raw errors that aren't *ProviderError)
// do NOT trigger failover — they're surfaced verbatim so a gateway-side
// defect isn't laundered through every registered vendor.
func shouldFailover(err error) bool {
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		return false
	}
	return pe.Retriable
}
