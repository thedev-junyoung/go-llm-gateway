package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/ratelimit"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/router"
)

// Config configures a Gateway. Only Providers is required in v0.1.
//
// Failover trigger conditions are fixed by ADR-004 (every Retriable=true
// *ProviderError triggers a switch to the next supporting provider, no
// in-provider retry, no inter-attempt backoff, caller's ctx is the only
// time budget). RateLimit / Metrics / Logging fields will be added later
// as non-breaking struct additions.
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
}

// Gateway is the composition root: it owns the providers and dispatches
// Chat through the routing layer. Construct with New, never via struct
// literal — future ADRs may add invariants (initialized rate limiter,
// metrics recorder, etc.) that the constructor enforces.
type Gateway struct {
	providers []provider.Provider
	rateLimit ratelimit.RateLimiter // nil means rate limiting disabled
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
	return &Gateway{
		providers: cfg.Providers,
		rateLimit: cfg.RateLimit,
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
// The router itself returns *provider.ProviderError with Vendor()=="gateway"
// for "no provider supports this model" (ADR-003 Q3), so call sites can
// distinguish routing failures from vendor-side rejections without parsing
// strings.
func (g *Gateway) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	primary, fallbacks, err := router.PickWithFallbacks(g.providers, req.Model)
	if err != nil {
		return provider.ChatResponse{}, err
	}

	candidates := make([]provider.Provider, 0, 1+len(fallbacks))
	candidates = append(candidates, primary)
	candidates = append(candidates, fallbacks...)

	var lastErr error
	for _, p := range candidates {
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
				// FailOpen: log and proceed. FailClosed wiring lands once a
				// backend that can actually fail (Redis) is in the tree.
				slog.WarnContext(ctx, "ratelimit backend error",
					"vendor", p.Name(), "err", lerr)
			} else if !d.Allow {
				pe := provider.NewProviderError(p.Name(), provider.ErrorTypeRateLimit, 0, true,
					"rate limited by gateway", nil)
				if d.RetryAfter != nil {
					pe = pe.WithRetryAfter(*d.RetryAfter)
				}
				lastErr = pe
				continue
			}
		}

		resp, cerr := p.Chat(ctx, req)
		if cerr == nil {
			if g.rateLimit != nil {
				if rerr := g.rateLimit.Record(ctx, p.Name(), p.KeyHash(), resp.Usage); rerr != nil {
					slog.WarnContext(ctx, "ratelimit record failed",
						"vendor", p.Name(), "err", rerr)
				}
			}
			return resp, nil
		}
		lastErr = cerr

		if !shouldFailover(cerr) {
			return provider.ChatResponse{}, cerr
		}
		// retriable — try the next candidate.
	}

	// Exhausted every supporting provider with retriable failures.
	return provider.ChatResponse{}, lastErr
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
