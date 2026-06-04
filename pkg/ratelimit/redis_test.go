package ratelimit_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/ratelimit"
)

// newRedisBackend spins up miniredis + a go-redis client wired to it and
// returns the backend plus a cleanup. Tests use this instead of a real
// Redis so they stay hermetic; miniredis ships its own Lua interpreter
// that matches Redis's EVAL semantics closely enough for sliding-window
// log code.
func newRedisBackend(t *testing.T, opts ratelimit.Options) (*ratelimit.RedisBackend, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = cli.Close() })
	return ratelimit.NewRedis(cli, opts), mr
}

func staticClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func reqWith(content string, maxTokens int) provider.ChatRequest {
	mt := maxTokens
	return provider.ChatRequest{
		Model:     "gpt-4o",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: content}},
		MaxTokens: &mt,
	}
}

func TestRedis_AllowsUnderLimit(t *testing.T) {
	t.Parallel()

	b, _ := newRedisBackend(t, ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 5, TokensPerMinute: 100_000},
		Clock:  staticClock(time.Unix(1_700_000_000, 0)),
	})

	for i := 0; i < 5; i++ {
		d, err := b.Allow(context.Background(), "openai", "hash-A", reqWith("hello", 100))
		if err != nil {
			t.Fatalf("Allow err = %v", err)
		}
		if !d.Allow {
			t.Fatalf("attempt %d: Allow = false, want true", i+1)
		}
	}
}

func TestRedis_RpmDeniesWhenExceeded(t *testing.T) {
	t.Parallel()

	b, _ := newRedisBackend(t, ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 2},
		Clock:  staticClock(time.Unix(1_700_000_000, 0)),
	})

	for i := 0; i < 2; i++ {
		d, err := b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 10))
		if err != nil || !d.Allow {
			t.Fatalf("attempt %d: Allow=%v err=%v", i+1, d.Allow, err)
		}
	}

	d, err := b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 10))
	if err != nil {
		t.Fatalf("3rd Allow err = %v", err)
	}
	if d.Allow {
		t.Fatal("3rd Allow = true, want false (RPM exceeded)")
	}
	if d.RetryAfter == nil {
		t.Fatal("RetryAfter = nil, want non-nil hint when denied")
	}
	if *d.RetryAfter < 0 || *d.RetryAfter > time.Minute {
		t.Errorf("RetryAfter = %v, want 0..60s", *d.RetryAfter)
	}
}

func TestRedis_TpmDeniesWhenExceeded(t *testing.T) {
	t.Parallel()

	// EstimateTokens of reqWith("hi", 500) ≈ 500 (input estimate is ~0
	// for 2 chars, output max = 500). Two attempts reserve ~1000; third
	// would push past 1000.
	b, _ := newRedisBackend(t, ratelimit.Options{
		Limits: ratelimit.Limits{TokensPerMinute: 1000},
		Clock:  staticClock(time.Unix(1_700_000_000, 0)),
	})

	for i := 0; i < 2; i++ {
		d, err := b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 500))
		if err != nil || !d.Allow {
			t.Fatalf("attempt %d: Allow=%v err=%v", i+1, d.Allow, err)
		}
	}

	d, err := b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 500))
	if err != nil {
		t.Fatalf("3rd Allow err = %v", err)
	}
	if d.Allow {
		t.Fatal("3rd Allow = true, want false (TPM exceeded)")
	}
}

func TestRedis_WindowExpiresAfter60s(t *testing.T) {
	t.Parallel()

	clk := time.Unix(1_700_000_000, 0)
	now := clk
	b, _ := newRedisBackend(t, ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 1},
		Clock:  func() time.Time { return now },
	})

	if d, err := b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 10)); err != nil || !d.Allow {
		t.Fatalf("first Allow: Allow=%v err=%v", d.Allow, err)
	}
	if d, err := b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 10)); err != nil || d.Allow {
		t.Fatalf("second Allow: Allow=%v err=%v (want denied)", d.Allow, err)
	}

	// Advance past the 60s window — the old entry should be pruned and
	// the next Allow should succeed.
	now = now.Add(61 * time.Second)
	if d, err := b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 10)); err != nil || !d.Allow {
		t.Fatalf("post-window Allow: Allow=%v err=%v", d.Allow, err)
	}
}

func TestRedis_PerProviderIsolation(t *testing.T) {
	t.Parallel()

	b, _ := newRedisBackend(t, ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 1},
		Clock:  staticClock(time.Unix(1_700_000_000, 0)),
	})

	// Exhaust openai's bucket.
	if d, err := b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 10)); err != nil || !d.Allow {
		t.Fatalf("openai first: Allow=%v err=%v", d.Allow, err)
	}
	if d, _ := b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 10)); d.Allow {
		t.Fatal("openai second: Allow = true, want false")
	}

	// anthropic should still have headroom — buckets are per-provider.
	if d, err := b.Allow(context.Background(), "anthropic", "hash-A", reqWith("hi", 10)); err != nil || !d.Allow {
		t.Fatalf("anthropic with same key hash should be independent: Allow=%v err=%v", d.Allow, err)
	}
}

func TestRedis_PerKeyIsolation(t *testing.T) {
	t.Parallel()

	b, _ := newRedisBackend(t, ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 1},
		Clock:  staticClock(time.Unix(1_700_000_000, 0)),
	})

	if d, err := b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 10)); err != nil || !d.Allow {
		t.Fatalf("keyA first: Allow=%v err=%v", d.Allow, err)
	}
	if d, _ := b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 10)); d.Allow {
		t.Fatal("keyA second: Allow = true, want false")
	}

	// Different API key → different bucket.
	if d, err := b.Allow(context.Background(), "openai", "hash-B", reqWith("hi", 10)); err != nil || !d.Allow {
		t.Fatalf("keyB should be independent of keyA: Allow=%v err=%v", d.Allow, err)
	}
}

func TestRedis_BothLimitsZeroShortCircuits(t *testing.T) {
	t.Parallel()

	b, mr := newRedisBackend(t, ratelimit.Options{
		Limits: ratelimit.Limits{}, // both zero
		Clock:  staticClock(time.Unix(1_700_000_000, 0)),
	})

	for i := 0; i < 10; i++ {
		d, err := b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 10))
		if err != nil || !d.Allow {
			t.Fatalf("attempt %d: Allow=%v err=%v", i+1, d.Allow, err)
		}
	}

	// The short-circuit avoids touching Redis at all, so no keys should
	// have been created. (Verifies the optimization, not just correctness.)
	if keys := mr.Keys(); len(keys) > 0 {
		t.Errorf("unconfigured limiter wrote %d keys, want 0: %v", len(keys), keys)
	}
}

func TestRedis_KeysExpire(t *testing.T) {
	t.Parallel()

	b, mr := newRedisBackend(t, ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 5},
		Clock:  staticClock(time.Unix(1_700_000_000, 0)),
	})

	if _, err := b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 10)); err != nil {
		t.Fatalf("Allow err = %v", err)
	}

	// Both keys should have a TTL set so abandoned buckets don't leak.
	for _, k := range mr.Keys() {
		if !strings.HasPrefix(k, "ratelimit:openai:hash-A:") {
			continue
		}
		ttl := mr.TTL(k)
		if ttl <= 0 || ttl > 61*time.Second {
			t.Errorf("key %q TTL = %v, want (0, 61s]", k, ttl)
		}
	}
}

func TestRedis_RecordIsNoOp(t *testing.T) {
	t.Parallel()

	b, _ := newRedisBackend(t, ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 5},
		Clock:  staticClock(time.Unix(1_700_000_000, 0)),
	})
	if err := b.Record(context.Background(), "openai", "hash-A", provider.Usage{InputTokens: 100, OutputTokens: 50}); err != nil {
		t.Errorf("Record err = %v, want nil (v0.1 no-op contract)", err)
	}
}

// TestRedis_ConcurrentSameSecondAreCountedIndependently exercises the
// nonce path: 20 goroutines racing the same bucket at the same second
// should each produce a unique ZSET member, otherwise some reservations
// dedupe and the bucket silently under-counts.
func TestRedis_ConcurrentSameSecondAreCountedIndependently(t *testing.T) {
	t.Parallel()

	b, mr := newRedisBackend(t, ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 100},
		Clock:  staticClock(time.Unix(1_700_000_000, 0)),
	})

	var wg sync.WaitGroup
	const N = 20
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 10))
		}()
	}
	wg.Wait()

	// All N attempts were within limit, so the RPM ZSET should hold N
	// distinct members. Under-count would mean nonces collided.
	for _, k := range mr.Keys() {
		if strings.HasSuffix(k, ":rpm") {
			got, err := mr.ZMembers(k)
			if err != nil {
				t.Fatalf("ZMembers(%q) err = %v", k, err)
			}
			if len(got) != N {
				t.Errorf("rpm ZSET size = %d, want %d (nonce collision under-counted)", len(got), N)
			}
		}
	}
}

func TestRedis_BackendErrorSurfaces(t *testing.T) {
	t.Parallel()

	// Point at a closed miniredis to simulate backend outage.
	mr := miniredis.RunT(t)
	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = cli.Close() })
	mr.Close()

	b := ratelimit.NewRedis(cli, ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 1},
		Clock:  staticClock(time.Unix(1_700_000_000, 0)),
	})

	_, err := b.Allow(context.Background(), "openai", "hash-A", reqWith("hi", 10))
	if err == nil {
		t.Fatal("Allow err = nil, want non-nil (backend down)")
	}
}
