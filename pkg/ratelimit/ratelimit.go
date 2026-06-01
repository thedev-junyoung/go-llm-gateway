// Package ratelimit implements gateway-side, sliding-window rate limiting
// against per-API-key vendor quotas. The package defines the RateLimiter
// contract (ADR-005 Synthesis); concrete backends — in-memory for
// single-instance dev/test, Redis for multi-instance production — live in
// peer files.
//
// See docs/adr/0005-distributed-rate-limit.md for the seven design
// decisions: pluggable backend, sliding window log, entry-reservation +
// after-response reconciliation, RPM+TPM both, per-provider × per-key
// granularity, fail-open default, same ErrRateLimited sentinel as vendor 429.
package ratelimit

import (
	"context"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// RateLimiter is the v0.1 contract every backend implements. Backends MUST
// honor the per-provider × per-key granularity defined in ADR-005 Q5.
type RateLimiter interface {
	// Allow asks whether req may proceed under the current window. Called
	// at gateway.Chat entry. If allowed, the limiter reserves a conservative
	// estimate (input tokens + max_tokens) against the window; the response
	// is reconciled via Record after the vendor call.
	Allow(ctx context.Context, providerName, apiKeyHash string, req provider.ChatRequest) (Decision, error)

	// Record reconciles the reservation with actual Usage from the vendor.
	// Called after a successful Chat. v0.1 in-memory backends MAY no-op here
	// (the conservative reservation stays in the window until expiry);
	// production Redis backends refund the (reservation − actual) difference
	// atomically.
	Record(ctx context.Context, providerName, apiKeyHash string, usage provider.Usage) error
}

// Decision is the limiter's verdict. RetryAfter is populated only when
// Allow==false; callers MAY surface it on a *provider.ProviderError via
// WithRetryAfter so failover and pre-empt paths produce identically shaped
// errors (ADR-005 Q7).
type Decision struct {
	Allow      bool
	RetryAfter *time.Duration
}

// Limits configures per-provider quotas. A zero value for a dimension means
// "unlimited" for that dimension — set to 0 to disable RPM-only or TPM-only
// checks (ADR-005 Q4).
type Limits struct {
	RequestsPerMinute int // RPM (zero = unlimited)
	TokensPerMinute   int // TPM, combined input + output tokens (zero = unlimited)
}

// FailMode selects the behavior when the backend itself errors (Redis down,
// network partition, etc.). Defaults to FailOpen — Library's job is vendor
// protection, not user gating; backend outage must not become service outage
// (ADR-005 Q6).
type FailMode int

const (
	// FailOpen allows the request to proceed when the backend errors.
	// Caller's vendor call still happens; vendor's own 429 is the safety net.
	FailOpen FailMode = iota

	// FailClosed denies the request when the backend errors. Opt in only
	// when cost protection trumps availability.
	FailClosed
)

// Options is the constructor parameter shared by every backend.
type Options struct {
	Limits   Limits
	FailMode FailMode

	// Clock is injectable for deterministic tests. nil falls back to time.Now.
	Clock func() time.Time
}

// now returns the configured Clock value or time.Now if Clock is nil.
func (o Options) now() time.Time {
	if o.Clock != nil {
		return o.Clock()
	}
	return time.Now()
}

// EstimateTokens returns a conservative entry-side token reservation for req:
// a char/4 heuristic on every Content string plus MaxTokens (or a 1024
// fallback when MaxTokens is nil). A real tokenizer (tiktoken / Anthropic
// claude-tokenizer) is a v0.2 follow-up — this heuristic over-estimates
// slightly, which is the safe direction for vendor protection.
func EstimateTokens(req provider.ChatRequest) int {
	chars := len(req.System)
	for _, m := range req.Messages {
		chars += len(m.Content)
	}
	inputEstimate := chars / 4

	output := 1024 // conservative default when MaxTokens is nil
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		output = *req.MaxTokens
	}
	return inputEstimate + output
}

// bucketKey is the per-provider × per-key composite key used by every
// backend. Backends MUST use this helper so the granularity stays consistent
// (ADR-005 Q5).
func bucketKey(providerName, apiKeyHash string) string {
	return providerName + ":" + apiKeyHash
}
