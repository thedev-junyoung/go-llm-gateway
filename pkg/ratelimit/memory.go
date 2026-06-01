package ratelimit

import (
	"context"
	"sync"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// MemoryBackend is the single-process RateLimiter. It implements the same
// sliding window log algorithm as the Redis backend will, but keeps the log
// in a process-local map guarded by sync.Mutex. Two consequences:
//
//   - Multi-instance deployments need the Redis backend instead — two
//     MemoryBackend instances on different processes don't share state.
//   - Record is a no-op here: the conservative reservation stays in the
//     window until it expires. Redis backend refunds the (reservation −
//     actual) difference inside a Lua script; reproducing that atomicity
//     across goroutines without overcomplicating the in-memory code isn't
//     worth it for v0.1 (over-conservative single-instance behavior is
//     safe — it errs toward under-utilization of the vendor quota, which
//     is the safe direction).
//
// MemoryBackend is safe for concurrent use.
type MemoryBackend struct {
	opts Options

	mu      sync.Mutex
	buckets map[string]*window // key = bucketKey(provider, apiKeyHash)
}

// window is the in-memory equivalent of the Redis ZSETs (one for RPM, one
// for TPM). entries stores both — each entry is a single reservation.
type window struct {
	entries []entry
}

type entry struct {
	at     time.Time
	tokens int
}

// NewMemory builds a MemoryBackend with the given options.
func NewMemory(opts Options) *MemoryBackend {
	return &MemoryBackend{
		opts:    opts,
		buckets: make(map[string]*window),
	}
}

// Allow checks the RPM and TPM windows; if either would exceed its limit
// it returns a deny Decision with the duration until the oldest in-window
// entry expires. On allow, it appends a conservative reservation entry.
func (m *MemoryBackend) Allow(_ context.Context, providerName, apiKeyHash string, req provider.ChatRequest) (Decision, error) {
	now := m.opts.now()
	cutoff := now.Add(-time.Minute)

	tokens := EstimateTokens(req)
	key := bucketKey(providerName, apiKeyHash)

	m.mu.Lock()
	defer m.mu.Unlock()

	w, ok := m.buckets[key]
	if !ok {
		w = &window{}
		m.buckets[key] = w
	}

	// Prune entries older than the window.
	w.entries = pruneBefore(w.entries, cutoff)

	rpmUsed := len(w.entries)
	tpmUsed := 0
	for _, e := range w.entries {
		tpmUsed += e.tokens
	}

	// RPM check (zero = unlimited).
	if lim := m.opts.Limits.RequestsPerMinute; lim > 0 && rpmUsed+1 > lim {
		retryAfter := nextSlotAvailable(w.entries, now)
		return Decision{Allow: false, RetryAfter: &retryAfter}, nil
	}
	// TPM check.
	if lim := m.opts.Limits.TokensPerMinute; lim > 0 && tpmUsed+tokens > lim {
		retryAfter := nextSlotAvailable(w.entries, now)
		return Decision{Allow: false, RetryAfter: &retryAfter}, nil
	}

	// Reserve.
	w.entries = append(w.entries, entry{at: now, tokens: tokens})
	return Decision{Allow: true}, nil
}

// Record is a no-op for MemoryBackend; see the type-level comment for why.
func (m *MemoryBackend) Record(_ context.Context, _, _ string, _ provider.Usage) error {
	return nil
}

// pruneBefore returns entries with at >= cutoff. Caller passes a slice the
// backend owns; this re-uses the underlying array.
func pruneBefore(entries []entry, cutoff time.Time) []entry {
	i := 0
	for ; i < len(entries); i++ {
		if !entries[i].at.Before(cutoff) {
			break
		}
	}
	if i == 0 {
		return entries
	}
	return append(entries[:0], entries[i:]...)
}

// nextSlotAvailable estimates the duration until the oldest in-window entry
// falls out — i.e. the soonest moment a new reservation could succeed for
// at least the RPM dimension. Conservative on the TPM dimension (a single
// large entry expiring may free enough room before the next entry).
func nextSlotAvailable(entries []entry, now time.Time) time.Duration {
	if len(entries) == 0 {
		return 0
	}
	d := time.Minute - now.Sub(entries[0].at)
	if d < 0 {
		return 0
	}
	return d
}

// Compile-time interface satisfaction.
var _ RateLimiter = (*MemoryBackend)(nil)
