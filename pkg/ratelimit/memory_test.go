package ratelimit_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/ratelimit"
)

// fixedClock makes time advance via Step; otherwise the limiter sees a
// stable "now" so tests don't race against real time.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(start time.Time) *fixedClock { return &fixedClock{now: start} }

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) Step(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// req builds a ChatRequest whose EstimateTokens result is roughly `wantTokens`.
// We compose it from Content (each char ≈ 1/4 token) + an explicit MaxTokens
// so callers can control the entry-side estimate precisely.
func req(model, content string, maxTokens int) provider.ChatRequest {
	return provider.ChatRequest{
		Model:     model,
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: content}},
		MaxTokens: &maxTokens,
	}
}

func TestMemory_Allow_NoLimits_AlwaysAllows(t *testing.T) {
	t.Parallel()

	m := ratelimit.NewMemory(ratelimit.Options{}) // zero limits = unlimited
	for i := 0; i < 100; i++ {
		d, err := m.Allow(context.Background(), "openai", "k", req("gpt-4o", "hello", 100))
		if err != nil {
			t.Fatalf("Allow #%d err = %v", i, err)
		}
		if !d.Allow {
			t.Fatalf("Allow #%d denied with no limits configured", i)
		}
	}
}

func TestMemory_Allow_RPMHits_Denies(t *testing.T) {
	t.Parallel()

	clock := newClock(time.Unix(1_700_000_000, 0))
	m := ratelimit.NewMemory(ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 3},
		Clock:  clock.Now,
	})

	// Three requests within the window: all allowed.
	for i := 0; i < 3; i++ {
		d, _ := m.Allow(context.Background(), "openai", "k", req("gpt-4o", "x", 10))
		if !d.Allow {
			t.Fatalf("Allow #%d denied; want allowed", i)
		}
	}

	// Fourth must deny with a RetryAfter equal to the remaining window.
	d, _ := m.Allow(context.Background(), "openai", "k", req("gpt-4o", "x", 10))
	if d.Allow {
		t.Fatal("Allow #4 allowed; want denied (RPM=3)")
	}
	if d.RetryAfter == nil {
		t.Fatal("RetryAfter nil on deny")
	}
	if *d.RetryAfter <= 0 || *d.RetryAfter > time.Minute {
		t.Errorf("RetryAfter = %v, want (0, 60s]", *d.RetryAfter)
	}
}

func TestMemory_Allow_TPMHits_Denies(t *testing.T) {
	t.Parallel()

	clock := newClock(time.Unix(1_700_000_000, 0))
	m := ratelimit.NewMemory(ratelimit.Options{
		Limits: ratelimit.Limits{TokensPerMinute: 5000},
		Clock:  clock.Now,
	})

	// First request reserves 1 char/4 + 4000 = 4000 tokens (under 5000).
	d, _ := m.Allow(context.Background(), "openai", "k", req("gpt-4o", "x", 4000))
	if !d.Allow {
		t.Fatal("first request denied unexpectedly")
	}

	// Second request would push total to 8000 → deny.
	d, _ = m.Allow(context.Background(), "openai", "k", req("gpt-4o", "x", 4000))
	if d.Allow {
		t.Fatal("second request allowed; want denied (TPM)")
	}
}

func TestMemory_Allow_WindowExpiry_AllowsAgain(t *testing.T) {
	t.Parallel()

	clock := newClock(time.Unix(1_700_000_000, 0))
	m := ratelimit.NewMemory(ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 1},
		Clock:  clock.Now,
	})

	d, _ := m.Allow(context.Background(), "openai", "k", req("gpt-4o", "x", 10))
	if !d.Allow {
		t.Fatal("first request denied")
	}
	d, _ = m.Allow(context.Background(), "openai", "k", req("gpt-4o", "x", 10))
	if d.Allow {
		t.Fatal("second request inside window allowed")
	}

	// Advance past the window.
	clock.Step(time.Minute + time.Second)
	d, _ = m.Allow(context.Background(), "openai", "k", req("gpt-4o", "x", 10))
	if !d.Allow {
		t.Fatal("post-window request denied; window should have expired")
	}
}

func TestMemory_Allow_PerProviderIsolation(t *testing.T) {
	t.Parallel()

	m := ratelimit.NewMemory(ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 1},
	})

	// OpenAI fills its quota.
	if d, _ := m.Allow(context.Background(), "openai", "k", req("gpt-4o", "x", 10)); !d.Allow {
		t.Fatal("openai first request denied")
	}
	if d, _ := m.Allow(context.Background(), "openai", "k", req("gpt-4o", "x", 10)); d.Allow {
		t.Fatal("openai second request allowed; want denied")
	}

	// Anthropic must still be allowed (different bucket key).
	if d, _ := m.Allow(context.Background(), "anthropic", "k", req("claude-opus-4-7", "x", 10)); !d.Allow {
		t.Fatal("anthropic blocked by openai's quota — buckets are leaking")
	}
}

func TestMemory_Allow_PerKeyIsolation(t *testing.T) {
	t.Parallel()

	m := ratelimit.NewMemory(ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 1},
	})

	// Same provider, different key hashes.
	if d, _ := m.Allow(context.Background(), "openai", "key-A", req("gpt-4o", "x", 10)); !d.Allow {
		t.Fatal("key-A first denied")
	}
	if d, _ := m.Allow(context.Background(), "openai", "key-A", req("gpt-4o", "x", 10)); d.Allow {
		t.Fatal("key-A second allowed; want denied")
	}
	if d, _ := m.Allow(context.Background(), "openai", "key-B", req("gpt-4o", "x", 10)); !d.Allow {
		t.Fatal("key-B blocked by key-A's quota — keys are leaking")
	}
}

func TestMemory_Record_IsNoop(t *testing.T) {
	t.Parallel()

	// The in-memory backend documents Record as a no-op; verify that the
	// reservation entry stays after Record is invoked.
	m := ratelimit.NewMemory(ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 1},
	})
	if d, _ := m.Allow(context.Background(), "openai", "k", req("gpt-4o", "x", 10)); !d.Allow {
		t.Fatal("first request denied")
	}
	if err := m.Record(context.Background(), "openai", "k", provider.Usage{InputTokens: 1, OutputTokens: 2}); err != nil {
		t.Fatalf("Record err = %v", err)
	}
	// Record didn't refund — second request inside the window is still denied.
	if d, _ := m.Allow(context.Background(), "openai", "k", req("gpt-4o", "x", 10)); d.Allow {
		t.Fatal("second request allowed after Record; in-memory backend should NOT refund (no-op contract)")
	}
}

func TestMemory_Allow_Concurrent_NoRace(t *testing.T) {
	t.Parallel()

	m := ratelimit.NewMemory(ratelimit.Options{
		Limits: ratelimit.Limits{RequestsPerMinute: 100},
	})

	var wg sync.WaitGroup
	var allowed int64
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := m.Allow(context.Background(), "openai", "k", req("gpt-4o", "x", 10))
			if err != nil {
				t.Errorf("Allow err = %v", err)
				return
			}
			if d.Allow {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&allowed); got != 100 {
		t.Errorf("allowed = %d, want 100 (limit) — concurrency leak", got)
	}
}

func TestEstimateTokens_HeuristicShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  provider.ChatRequest
		min  int // EstimateTokens >= min
	}{
		{"empty req with nil MaxTokens uses 1024 default",
			provider.ChatRequest{}, 1024},
		{"messages chars contribute (char/4)",
			req("gpt-4o", "12345678", 100), 100 + 2},
		{"system chars contribute",
			provider.ChatRequest{System: "abcdefgh", MaxTokens: ptr(100)}, 100 + 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ratelimit.EstimateTokens(tc.req); got < tc.min {
				t.Errorf("EstimateTokens = %d, want >= %d", got, tc.min)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
