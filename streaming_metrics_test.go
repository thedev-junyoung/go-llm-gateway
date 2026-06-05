package gateway_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	gateway "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/metrics"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// captureStreamingRecorder records every TTFT and stream_duration call
// so tests can assert on outcome label resolution + emit frequency.
// Wraps the existing captureRecorder so it also satisfies the base
// MetricRecorder interface — the gateway wires through type assertion.
type captureStreamingRecorder struct {
	captureRecorder

	mu       sync.Mutex
	ttft     []observation
	duration []observation
}

type observation struct {
	vendor, model, outcome string
	d                      time.Duration
}

func (c *captureStreamingRecorder) ObserveFirstTokenLatency(vendor, model, outcome string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ttft = append(c.ttft, observation{vendor, model, outcome, d})
}

func (c *captureStreamingRecorder) ObserveStreamDuration(vendor, model, outcome string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.duration = append(c.duration, observation{vendor, model, outcome, d})
}

func (c *captureStreamingRecorder) snapshotTTFT() []observation {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]observation, len(c.ttft))
	copy(out, c.ttft)
	return out
}

func (c *captureStreamingRecorder) snapshotDuration() []observation {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]observation, len(c.duration))
	copy(out, c.duration)
	return out
}

var _ metrics.StreamingMetricRecorder = (*captureStreamingRecorder)(nil)

// drainStream reads chunks until the channel closes. Wraps the
// previously inlined `for range stream {}` so linters don't flag the
// empty body — these tests assert on side effects (recorder
// observations), not chunk content.
func drainStream(stream <-chan provider.StreamChunk) {
	for chunk := range stream {
		_ = chunk
	}
}

func TestChatStream_Metrics_Success_EmitsTTFTAndStreamDurationOnce(t *testing.T) {
	t.Parallel()

	p := newFakeStreaming("openai", []string{"gpt-4o"},
		[]provider.StreamChunk{
			{ContentDelta: "Hello"},
			{ContentDelta: " world"},
			{FinishReason: provider.FinishStop, Usage: provider.Usage{InputTokens: 5, OutputTokens: 2}},
		}, nil)

	rec := &captureStreamingRecorder{}
	gw, _ := gateway.New(gateway.Config{
		Providers:   []provider.Provider{p},
		Metrics:     rec,
		KnownModels: []string{"gpt-4o"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := gw.ChatStream(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}
	drainStream(stream)

	ttft := rec.snapshotTTFT()
	if len(ttft) != 1 {
		t.Fatalf("TTFT observations = %d, want 1", len(ttft))
	}
	if ttft[0].outcome != metrics.StreamOutcomeSuccess {
		t.Errorf("TTFT outcome = %q, want %q", ttft[0].outcome, metrics.StreamOutcomeSuccess)
	}
	if ttft[0].vendor != "openai" || ttft[0].model != "gpt-4o" {
		t.Errorf("TTFT labels = (%q, %q), want (openai, gpt-4o)", ttft[0].vendor, ttft[0].model)
	}

	dur := rec.snapshotDuration()
	if len(dur) != 1 {
		t.Fatalf("stream_duration observations = %d, want 1", len(dur))
	}
	if dur[0].outcome != metrics.StreamOutcomeSuccess {
		t.Errorf("stream_duration outcome = %q, want %q", dur[0].outcome, metrics.StreamOutcomeSuccess)
	}
}

// TestChatStream_Metrics_PreStreamFailure_FailoverEmitsBothCandidates
// pins ADR-007 Q5: every ChatStream attempt — even the ones that
// fail before a chunk arrives — appears in the TTFT histogram with
// outcome=pre_stream_failure. Without this, the failover path is
// invisible to TTFT dashboards.
func TestChatStream_Metrics_PreStreamFailure_FailoverEmitsBothCandidates(t *testing.T) {
	t.Parallel()

	primary := newFakeStreaming("openai", []string{"gpt-4o"}, nil,
		provider.NewProviderError("openai", provider.ErrorTypeRateLimit, 429, true, "throttled", nil))
	fallback := newFakeStreaming("azure-openai", []string{"gpt-4o"},
		[]provider.StreamChunk{
			{ContentDelta: "from fallback"},
			{FinishReason: provider.FinishStop},
		}, nil)

	rec := &captureStreamingRecorder{}
	gw, _ := gateway.New(gateway.Config{
		Providers:   []provider.Provider{primary, fallback},
		Metrics:     rec,
		KnownModels: []string{"gpt-4o"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := gw.ChatStream(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}
	drainStream(stream)

	ttft := rec.snapshotTTFT()
	if len(ttft) != 2 {
		t.Fatalf("TTFT observations = %d, want 2 (one per candidate)", len(ttft))
	}
	// First TTFT must be primary's pre-stream failure.
	if ttft[0].vendor != "openai" || ttft[0].outcome != metrics.StreamOutcomePreStreamFailure {
		t.Errorf("first TTFT = (%q, %q), want (openai, pre_stream_failure)", ttft[0].vendor, ttft[0].outcome)
	}
	// Second TTFT must be fallback's success.
	if ttft[1].vendor != "azure-openai" || ttft[1].outcome != metrics.StreamOutcomeSuccess {
		t.Errorf("second TTFT = (%q, %q), want (azure-openai, success)", ttft[1].vendor, ttft[1].outcome)
	}
}

// TestChatStream_Metrics_MidStreamError_StreamDurationOutcome pins the
// distinction Q5 requires: if some content arrived (TTFT=success) but
// the stream terminated with an Err chunk, stream_duration outcome
// is mid_stream_error — not just "error" — so operators can quantify
// "how often does the stream break after some output?".
func TestChatStream_Metrics_MidStreamError_StreamDurationOutcome(t *testing.T) {
	t.Parallel()

	pe := provider.NewProviderError("openai", provider.ErrorTypeServer, 500, true, "vendor exploded", nil)
	p := newFakeStreaming("openai", []string{"gpt-4o"},
		[]provider.StreamChunk{
			{ContentDelta: "partial"},
			{FinishReason: provider.FinishUnknown, Err: pe},
		}, nil)

	rec := &captureStreamingRecorder{}
	gw, _ := gateway.New(gateway.Config{
		Providers:   []provider.Provider{p},
		Metrics:     rec,
		KnownModels: []string{"gpt-4o"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := gw.ChatStream(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}
	drainStream(stream)

	ttft := rec.snapshotTTFT()
	if len(ttft) != 1 || ttft[0].outcome != metrics.StreamOutcomeSuccess {
		t.Errorf("TTFT = %+v, want one success entry (first chunk did arrive)", ttft)
	}
	dur := rec.snapshotDuration()
	if len(dur) != 1 || dur[0].outcome != metrics.StreamOutcomeMidStreamError {
		t.Errorf("stream_duration = %+v, want one mid_stream_error", dur)
	}
}

// TestChatStream_Metrics_NormalizesUnknownModel pins ADR-006 Q7's
// "unknown" normalization on the streaming path — if the requested
// model isn't in KnownModels, both histograms see the "unknown"
// label, matching the sync path's behavior.
func TestChatStream_Metrics_NormalizesUnknownModel(t *testing.T) {
	t.Parallel()

	p := newFakeStreaming("openai", []string{"weird-model-id"},
		[]provider.StreamChunk{
			{ContentDelta: "x"},
			{FinishReason: provider.FinishStop},
		}, nil)

	rec := &captureStreamingRecorder{}
	gw, _ := gateway.New(gateway.Config{
		Providers: []provider.Provider{p},
		Metrics:   rec,
		// No KnownModels → strict default: every model collapses to "unknown".
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := gw.ChatStream(ctx, provider.ChatRequest{
		Model:    "weird-model-id",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}
	drainStream(stream)

	ttft := rec.snapshotTTFT()
	if len(ttft) != 1 || ttft[0].model != "unknown" {
		t.Errorf("TTFT model = %v, want unknown (KnownModels nil collapses everything)", ttft)
	}
	dur := rec.snapshotDuration()
	if len(dur) != 1 || dur[0].model != "unknown" {
		t.Errorf("stream_duration model = %v, want unknown", dur)
	}
}

// TestChatStream_Metrics_SyncOnlyRecorder_NoStreamMetrics pins the
// type-assertion gate — a recorder that satisfies MetricRecorder but
// NOT StreamingMetricRecorder must be silently skipped on the
// streaming path, not throw a runtime error.
func TestChatStream_Metrics_SyncOnlyRecorder_NoStreamMetrics(t *testing.T) {
	t.Parallel()

	p := newFakeStreaming("openai", []string{"gpt-4o"},
		[]provider.StreamChunk{
			{ContentDelta: "ok"},
			{FinishReason: provider.FinishStop},
		}, nil)

	// captureRecorder (from metrics_test.go) implements MetricRecorder
	// but NOT StreamingMetricRecorder — exactly the v0.1 caller case.
	rec := &captureRecorder{}
	gw, _ := gateway.New(gateway.Config{
		Providers: []provider.Provider{p},
		Metrics:   rec,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := gw.ChatStream(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}
	drainStream(stream)
	// Test passes if we got here without panic — sync-only recorder
	// silently skipped streaming metric calls.
}

func TestChatStream_Metrics_NoMetricsRecorder_StillWorks(t *testing.T) {
	t.Parallel()

	p := newFakeStreaming("openai", []string{"gpt-4o"},
		[]provider.StreamChunk{
			{ContentDelta: "ok"},
			{FinishReason: provider.FinishStop},
		}, nil)

	gw, _ := gateway.New(gateway.Config{Providers: []provider.Provider{p}}) // Metrics nil → NoOp

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := gw.ChatStream(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}
	count := 0
	for range stream {
		count++
	}
	if count != 2 {
		t.Errorf("chunks received = %d, want 2", count)
	}
}

// TestChatStream_Metrics_AllPreStreamFailures_NoStreamReached pins the
// edge case where every candidate fails pre-stream — TTFT histogram
// has N pre_stream_failure observations, but stream_duration is never
// emitted (no stream was wrapped). The error propagates to the caller.
func TestChatStream_Metrics_AllPreStreamFailures_NoStreamReached(t *testing.T) {
	t.Parallel()

	makeFailing := func(name string) *fakeStreamingProvider {
		return newFakeStreaming(name, []string{"gpt-4o"}, nil,
			provider.NewProviderError(name, provider.ErrorTypeServer, 503, true, "down", nil))
	}

	rec := &captureStreamingRecorder{}
	gw, _ := gateway.New(gateway.Config{
		Providers: []provider.Provider{makeFailing("openai"), makeFailing("azure-openai")},
		Metrics:   rec,
	})

	_, err := gw.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error after all candidates fail")
	}
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %v", err)
	}

	ttft := rec.snapshotTTFT()
	if len(ttft) != 2 {
		t.Errorf("TTFT observations = %d, want 2 (one pre_stream_failure per candidate)", len(ttft))
	}
	for i, obs := range ttft {
		if obs.outcome != metrics.StreamOutcomePreStreamFailure {
			t.Errorf("TTFT[%d] outcome = %q, want pre_stream_failure", i, obs.outcome)
		}
	}
	// stream_duration is never observed when no stream is wrapped.
	if dur := rec.snapshotDuration(); len(dur) != 0 {
		t.Errorf("stream_duration observations = %d, want 0 (no stream wrapped)", len(dur))
	}
}
