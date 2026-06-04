package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// TestStreamChunk_ZeroValue pins the empty-state contract — every field
// has a useful zero value so callers can build chunks without ceremony.
// Catches a future refactor that adds a required field by accident.
func TestStreamChunk_ZeroValue(t *testing.T) {
	t.Parallel()
	var c provider.StreamChunk
	if c.ContentDelta != "" {
		t.Errorf("ContentDelta zero = %q, want empty", c.ContentDelta)
	}
	if c.FinishReason != "" {
		t.Errorf("FinishReason zero = %q, want empty", c.FinishReason)
	}
	if c.Usage.InputTokens != 0 || c.Usage.OutputTokens != 0 {
		t.Errorf("Usage zero = %+v, want zero", c.Usage)
	}
	if c.Raw != nil {
		t.Errorf("Raw zero = %v, want nil", c.Raw)
	}
	if c.Err != nil {
		t.Errorf("Err zero = %v, want nil", c.Err)
	}
}

// stubStreamingProvider satisfies StreamingProvider with a synthetic
// stream. Used to verify the type-assertion pattern callers will use.
type stubStreamingProvider struct{}

func (stubStreamingProvider) Name() string              { return "stub" }
func (stubStreamingProvider) SupportsModel(string) bool { return true }
func (stubStreamingProvider) KeyHash() string           { return "" }
func (stubStreamingProvider) Chat(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, nil
}

func (stubStreamingProvider) ChatStream(_ context.Context, _ provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	out := make(chan provider.StreamChunk, 1)
	out <- provider.StreamChunk{ContentDelta: "hi", FinishReason: provider.FinishStop}
	close(out)
	return out, nil
}

var (
	_ provider.Provider          = stubStreamingProvider{}
	_ provider.StreamingProvider = stubStreamingProvider{}
)

// stubSyncOnlyProvider satisfies Provider but NOT StreamingProvider —
// the deliberate v0.1-compat case (an adapter the author hasn't
// extended yet still works).
type stubSyncOnlyProvider struct{}

func (stubSyncOnlyProvider) Name() string              { return "sync-only" }
func (stubSyncOnlyProvider) SupportsModel(string) bool { return true }
func (stubSyncOnlyProvider) KeyHash() string           { return "" }
func (stubSyncOnlyProvider) Chat(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, nil
}

var _ provider.Provider = stubSyncOnlyProvider{}

// TestTypeAssertion_DistinguishesStreamingFromSync pins the consumer
// pattern the gateway's ChatStream will use to filter candidates.
func TestTypeAssertion_DistinguishesStreamingFromSync(t *testing.T) {
	t.Parallel()

	var streaming provider.Provider = stubStreamingProvider{}
	var syncOnly provider.Provider = stubSyncOnlyProvider{}

	if _, ok := streaming.(provider.StreamingProvider); !ok {
		t.Error("streaming provider failed StreamingProvider type assertion")
	}
	if _, ok := syncOnly.(provider.StreamingProvider); ok {
		t.Error("sync-only provider unexpectedly satisfies StreamingProvider")
	}
}

// TestStreamChunk_RawMarshalsRoundtrip guards against a future change
// that drops json.RawMessage in favor of []byte — callers depend on
// the marshal-friendly behavior to forward chunks into log pipelines
// without re-encoding.
func TestStreamChunk_RawMarshalsRoundtrip(t *testing.T) {
	t.Parallel()
	c := provider.StreamChunk{
		ContentDelta: "hello",
		Raw:          json.RawMessage(`{"event":"content_block_delta"}`),
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal err = %v", err)
	}
	var got provider.StreamChunk
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal err = %v", err)
	}
	if got.ContentDelta != "hello" {
		t.Errorf("ContentDelta after roundtrip = %q", got.ContentDelta)
	}
	if string(got.Raw) != `{"event":"content_block_delta"}` {
		t.Errorf("Raw after roundtrip = %q", got.Raw)
	}
}

// TestStreamChunk_ErrField_ProviderErrorCompatible verifies the Err
// field cooperates with the existing sentinel error pattern. Callers
// who already use errors.As(chunk.Err, &pe) on sync Chat errors
// shouldn't need a different idiom for streams.
func TestStreamChunk_ErrField_ProviderErrorCompatible(t *testing.T) {
	t.Parallel()
	pe := provider.NewProviderError("openai", provider.ErrorTypeRateLimit, 429, true, "throttled", nil)
	// FinishReason MUST be FinishUnknown when Err is non-nil per the
	// godoc contract — building the test fixture the same way an
	// adapter would catches a future drift where a producer forgets
	// to set the field.
	c := provider.StreamChunk{
		FinishReason: provider.FinishUnknown,
		Err:          pe,
	}

	if !errors.Is(c.Err, provider.ErrRateLimited) {
		t.Error("errors.Is(chunk.Err, ErrRateLimited) = false, want true")
	}
	var got *provider.ProviderError
	if !errors.As(c.Err, &got) {
		t.Fatal("errors.As(chunk.Err, &*ProviderError) failed")
	}
	if got.Type != provider.ErrorTypeRateLimit {
		t.Errorf("Type = %q", got.Type)
	}
}
