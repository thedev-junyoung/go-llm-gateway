package gateway_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	gateway "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/ratelimit"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/types"
)

// captureRecorder collects every emission for table-driven assertion. Mutex
// because the gateway invokes hooks synchronously from the Chat goroutine but
// tests inspect the slices from the main goroutine, and `-race` will catch
// any future refactor that introduces async dispatch.
type captureRecorder struct {
	mu        sync.Mutex
	attempts  []provider.AttemptInfo
	failovers []provider.FailoverInfo
	panicNext bool
}

func (c *captureRecorder) OnAttempt(_ context.Context, info provider.AttemptInfo) {
	if c.panicNext {
		c.panicNext = false
		panic("captureRecorder: induced panic")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts = append(c.attempts, info)
}

func (c *captureRecorder) OnFailover(_ context.Context, info provider.FailoverInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failovers = append(c.failovers, info)
}

func (c *captureRecorder) snapshotAttempts() []provider.AttemptInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]provider.AttemptInfo, len(c.attempts))
	copy(out, c.attempts)
	return out
}

func (c *captureRecorder) snapshotFailovers() []provider.FailoverInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]provider.FailoverInfo, len(c.failovers))
	copy(out, c.failovers)
	return out
}

func TestMetrics_Success_EmitsVendorAttempt(t *testing.T) {
	t.Parallel()

	p := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{
				Content: "ok",
				Usage:   provider.Usage{InputTokens: 7, OutputTokens: 3},
			}, nil
		})

	rec := &captureRecorder{}
	gw, err := gateway.New(gateway.Config{
		Providers: []provider.Provider{p},
		Metrics:   rec,
	})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}

	resp, err := gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat err = %v", err)
	}

	att := rec.snapshotAttempts()
	if len(att) != 1 {
		t.Fatalf("len(attempts) = %d, want 1", len(att))
	}
	got := att[0]
	if got.Vendor != "openai" {
		t.Errorf("Vendor = %q, want %q", got.Vendor, "openai")
	}
	if got.Origin != types.OriginVendor {
		t.Errorf("Origin = %q, want %q", got.Origin, types.OriginVendor)
	}
	if got.Outcome != types.OutcomeSuccess {
		t.Errorf("Outcome = %q, want %q", got.Outcome, types.OutcomeSuccess)
	}
	if got.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", got.Duration)
	}
	if got.Usage.InputTokens != 7 || got.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v, want {7,3}", got.Usage)
	}
	if got.Error != nil {
		t.Errorf("Error = %v, want nil on success", got.Error)
	}

	// Same record must appear in ChatResponse.Attempts so callers can join
	// by request_id without subscribing to the recorder. See ADR-006 Q4.
	if len(resp.Attempts) != 1 || resp.Attempts[0] != got {
		t.Errorf("ChatResponse.Attempts mismatch: resp=%+v recorder=%+v", resp.Attempts, got)
	}
}

func TestMetrics_VendorError_OutcomeAndErrorPopulated(t *testing.T) {
	t.Parallel()

	pe := provider.NewProviderError("openai", provider.ErrorTypeRateLimit, 429, true, "throttled", nil)
	p := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{}, pe
		})

	rec := &captureRecorder{}
	gw, _ := gateway.New(gateway.Config{Providers: []provider.Provider{p}, Metrics: rec})

	_, err := gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}

	att := rec.snapshotAttempts()
	if len(att) != 1 {
		t.Fatalf("len(attempts) = %d, want 1", len(att))
	}
	got := att[0]
	if got.Outcome != types.OutcomeErrorRateLimit {
		t.Errorf("Outcome = %q, want %q", got.Outcome, types.OutcomeErrorRateLimit)
	}
	if got.Origin != types.OriginVendor {
		t.Errorf("Origin = %q, want %q", got.Origin, types.OriginVendor)
	}
	if got.Error != pe {
		t.Errorf("Error = %v, want the *ProviderError emitted by the adapter", got.Error)
	}
}

func TestMetrics_RouterFailure_EmitsSyntheticGatewayAttempt(t *testing.T) {
	t.Parallel()

	p := newFake("openai", []string{"gpt-4o"}, nil)
	rec := &captureRecorder{}
	gw, _ := gateway.New(gateway.Config{Providers: []provider.Provider{p}, Metrics: rec})

	_, err := gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gemini-2.0-pro",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected router error")
	}

	att := rec.snapshotAttempts()
	if len(att) != 1 {
		t.Fatalf("len(attempts) = %d, want 1", len(att))
	}
	got := att[0]
	if got.Vendor != "gateway" {
		t.Errorf("Vendor = %q, want %q (router-origin synthetic)", got.Vendor, "gateway")
	}
	if got.Origin != types.OriginGatewayRouter {
		t.Errorf("Origin = %q, want %q", got.Origin, types.OriginGatewayRouter)
	}
	if got.Outcome != types.OutcomeErrorInvalidInput {
		t.Errorf("Outcome = %q, want %q", got.Outcome, types.OutcomeErrorInvalidInput)
	}
	if got.Duration != 0 {
		t.Errorf("Duration = %v, want 0 (router never reached a vendor)", got.Duration)
	}
}

func TestMetrics_RateLimitPreempt_EmitsGatewayPreemptOrigin(t *testing.T) {
	t.Parallel()

	p := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			t.Fatal("provider.Chat must not run when preempt denies")
			return provider.ChatResponse{}, nil
		})
	lim := &fakeLimiter{
		allowFn: func(string) (ratelimit.Decision, error) {
			return ratelimit.Decision{Allow: false}, nil
		},
	}

	rec := &captureRecorder{}
	gw, _ := gateway.New(gateway.Config{
		Providers: []provider.Provider{p},
		RateLimit: lim,
		Metrics:   rec,
	})

	_, err := gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, provider.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}

	att := rec.snapshotAttempts()
	if len(att) != 1 {
		t.Fatalf("len(attempts) = %d, want 1", len(att))
	}
	got := att[0]
	if got.Origin != types.OriginGatewayPreempt {
		t.Errorf("Origin = %q, want %q", got.Origin, types.OriginGatewayPreempt)
	}
	if got.Outcome != types.OutcomeErrorRateLimit {
		t.Errorf("Outcome = %q, want %q", got.Outcome, types.OutcomeErrorRateLimit)
	}
	if got.Vendor != "openai" {
		t.Errorf("Vendor = %q, want %q", got.Vendor, "openai")
	}
	if got.Duration != 0 {
		t.Errorf("Duration = %v, want 0 (preempt never called the vendor)", got.Duration)
	}
}

func TestMetrics_Failover_EmitsFailoverInfo(t *testing.T) {
	t.Parallel()

	primary := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{}, provider.NewProviderError(
				"openai", provider.ErrorTypeRateLimit, 429, true, "throttled", nil)
		})
	fallback := newFake("azure-openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{Content: "ok"}, nil
		})

	rec := &captureRecorder{}
	gw, _ := gateway.New(gateway.Config{
		Providers: []provider.Provider{primary, fallback},
		Metrics:   rec,
	})

	_, err := gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat err = %v", err)
	}

	fos := rec.snapshotFailovers()
	if len(fos) != 1 {
		t.Fatalf("len(failovers) = %d, want 1", len(fos))
	}
	got := fos[0]
	if got.FromVendor != "openai" {
		t.Errorf("FromVendor = %q, want %q", got.FromVendor, "openai")
	}
	if got.ToVendor != "azure-openai" {
		t.Errorf("ToVendor = %q, want %q", got.ToVendor, "azure-openai")
	}
	if got.Reason != provider.ErrorTypeRateLimit {
		t.Errorf("Reason = %q, want %q", got.Reason, provider.ErrorTypeRateLimit)
	}
}

// TestMetrics_FailoverExhaustion_NoTerminalFailover guards against synthesizing
// a "failover to nowhere" when the last candidate also fails — there's no
// next vendor to hand off to, so no FailoverInfo should be emitted.
func TestMetrics_FailoverExhaustion_NoTerminalFailover(t *testing.T) {
	t.Parallel()

	makeFailing := func(name string) *fakeProvider {
		return newFake(name, []string{"gpt-4o"},
			func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
				return provider.ChatResponse{}, provider.NewProviderError(
					name, provider.ErrorTypeOverloaded, 503, true, "down", nil)
			})
	}

	rec := &captureRecorder{}
	gw, _ := gateway.New(gateway.Config{
		Providers: []provider.Provider{makeFailing("openai"), makeFailing("azure-openai")},
		Metrics:   rec,
	})

	_, err := gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error after exhausting all candidates")
	}

	// Two attempts (one per candidate), but only one failover (openai → azure).
	// The azure failure has no next candidate so no failover from it.
	if got := len(rec.snapshotAttempts()); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
	if got := len(rec.snapshotFailovers()); got != 1 {
		t.Errorf("failovers = %d, want 1 (no terminal failover from the last candidate)", got)
	}
}

func TestMetrics_KnownModels_UnknownNormalized(t *testing.T) {
	t.Parallel()

	p := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{Content: "ok"}, nil
		})

	rec := &captureRecorder{}
	gw, _ := gateway.New(gateway.Config{
		Providers:   []provider.Provider{p},
		Metrics:     rec,
		KnownModels: []string{"claude-opus-4-7"}, // gpt-4o intentionally missing
	})

	_, err := gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat err = %v", err)
	}

	att := rec.snapshotAttempts()
	if att[0].Model != "unknown" {
		t.Errorf("Model = %q, want %q (gpt-4o is not in KnownModels)", att[0].Model, "unknown")
	}
}

func TestMetrics_KnownModels_KnownPassesThrough(t *testing.T) {
	t.Parallel()

	p := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{Content: "ok"}, nil
		})

	rec := &captureRecorder{}
	gw, _ := gateway.New(gateway.Config{
		Providers:   []provider.Provider{p},
		Metrics:     rec,
		KnownModels: []string{"gpt-4o"},
	})

	_, err := gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat err = %v", err)
	}

	att := rec.snapshotAttempts()
	if att[0].Model != "gpt-4o" {
		t.Errorf("Model = %q, want %q (in KnownModels)", att[0].Model, "gpt-4o")
	}
}

// TestMetrics_NilKnownModels_StrictDefault pins ADR-006 Q7's strict default:
// when KnownModels is nil, EVERY model collapses to "unknown" — cardinality
// protection + explicit alert signal for new vendor model ids. Operators
// MUST register their models or accept the unknown bucket.
func TestMetrics_NilKnownModels_StrictDefault(t *testing.T) {
	t.Parallel()

	p := newFake("openai", []string{"weird-vendor-model-id"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{Content: "ok"}, nil
		})

	rec := &captureRecorder{}
	gw, _ := gateway.New(gateway.Config{
		Providers: []provider.Provider{p},
		Metrics:   rec,
		// KnownModels: nil — strict default
	})

	_, err := gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "weird-vendor-model-id",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat err = %v", err)
	}

	att := rec.snapshotAttempts()
	if att[0].Model != "unknown" {
		t.Errorf("Model = %q, want %q (strict default — register or unknown)", att[0].Model, "unknown")
	}
}

// TestMetrics_RecorderPanic_DoesNotCrashChat verifies the recover() guard
// inside the gateway's recordAttempt — a recorder that panics must not
// propagate up to the caller. Mirrors the same protection pkg/metrics
// MultiRecorder already provides internally, but the gateway re-runs it
// because callers can pass any MetricRecorder (including bare ones that
// have no panic guard of their own).
func TestMetrics_RecorderPanic_DoesNotCrashChat(t *testing.T) {
	t.Parallel()

	p := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{Content: "ok"}, nil
		})

	rec := &captureRecorder{panicNext: true}
	gw, _ := gateway.New(gateway.Config{Providers: []provider.Provider{p}, Metrics: rec})

	resp, err := gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat err = %v, want nil (panic must be contained)", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Content = %q, want %q", resp.Content, "ok")
	}
}

// TestMetrics_NilConfig_DefaultsToNoOp verifies the documented invariant:
// the gateway installs NoOpRecorder when Config.Metrics is nil so the Chat
// loop can invoke hooks unconditionally without per-call nil checks.
// Indirect assertion: Chat must succeed with no recorder configured.
func TestMetrics_NilConfig_DefaultsToNoOp(t *testing.T) {
	t.Parallel()

	p := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{Content: "ok"}, nil
		})

	gw, _ := gateway.New(gateway.Config{Providers: []provider.Provider{p}}) // no Metrics

	resp, err := gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat err = %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Content = %q", resp.Content)
	}
}

// TestMetrics_ErrorPath_AttemptsEmpty pins the asymmetry from ADR-006 Q4:
// on success ChatResponse.Attempts mirrors the recorder trace, but on error
// it's left empty — the recorder is the canonical sink for failures.
func TestMetrics_ErrorPath_AttemptsEmpty(t *testing.T) {
	t.Parallel()

	pe := provider.NewProviderError("openai", provider.ErrorTypeAuth, 401, false, "bad key", nil)
	p := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{}, pe
		})

	rec := &captureRecorder{}
	gw, _ := gateway.New(gateway.Config{Providers: []provider.Provider{p}, Metrics: rec})

	resp, err := gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if len(resp.Attempts) != 0 {
		t.Errorf("Attempts on error path = %d, want 0", len(resp.Attempts))
	}
	// But recorder still received the attempt.
	if got := len(rec.snapshotAttempts()); got != 1 {
		t.Errorf("recorder attempts = %d, want 1 (recorder is canonical sink on error)", got)
	}
}
