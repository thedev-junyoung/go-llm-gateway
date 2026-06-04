package gateway_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	gateway "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// fakeStreamingProvider satisfies StreamingProvider with a programmable
// ChatStream. The chunks slice is sent in order then the channel
// closes; chatStreamErr lets a test inject a pre-stream failure.
type fakeStreamingProvider struct {
	name           string
	models         map[string]struct{}
	chunks         []provider.StreamChunk
	chatStreamErr  error
	chatStreamCall int
}

func newFakeStreaming(name string, models []string, chunks []provider.StreamChunk, err error) *fakeStreamingProvider {
	m := make(map[string]struct{}, len(models))
	for _, model := range models {
		m[model] = struct{}{}
	}
	return &fakeStreamingProvider{name: name, models: m, chunks: chunks, chatStreamErr: err}
}

func (f *fakeStreamingProvider) Name() string                    { return f.name }
func (f *fakeStreamingProvider) SupportsModel(model string) bool { _, ok := f.models[model]; return ok }
func (f *fakeStreamingProvider) KeyHash() string                 { return "fake-" + f.name }

func (f *fakeStreamingProvider) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, errors.New("Chat not used in streaming tests")
}

func (f *fakeStreamingProvider) ChatStream(_ context.Context, _ provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	f.chatStreamCall++
	if f.chatStreamErr != nil {
		return nil, f.chatStreamErr
	}
	out := make(chan provider.StreamChunk, len(f.chunks))
	for _, ch := range f.chunks {
		out <- ch
	}
	close(out)
	return out, nil
}

var _ provider.StreamingProvider = (*fakeStreamingProvider)(nil)

func TestChatStream_SingleStreamingProvider_ChunksForward(t *testing.T) {
	t.Parallel()

	p := newFakeStreaming("openai", []string{"gpt-4o"},
		[]provider.StreamChunk{
			{ContentDelta: "Hello"},
			{ContentDelta: ", "},
			{ContentDelta: "world"},
			{FinishReason: provider.FinishStop, Usage: provider.Usage{InputTokens: 5, OutputTokens: 3}},
		}, nil)

	gw, err := gateway.New(gateway.Config{Providers: []provider.Provider{p}})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := gw.ChatStream(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}

	var joined strings.Builder
	var terminal provider.StreamChunk
	for chunk := range stream {
		if chunk.ContentDelta != "" {
			joined.WriteString(chunk.ContentDelta)
		}
		if chunk.FinishReason != "" {
			terminal = chunk
		}
	}
	if joined.String() != "Hello, world" {
		t.Errorf("joined = %q", joined.String())
	}
	if terminal.FinishReason != provider.FinishStop {
		t.Errorf("terminal FinishReason = %q", terminal.FinishReason)
	}
}

// TestChatStream_PreStreamFailover pins ADR-007 Q4: a retriable
// pre-stream error on the primary falls over to the next streaming
// candidate. The fallback's stream reaches the caller as if it were
// the only attempt.
func TestChatStream_PreStreamFailover(t *testing.T) {
	t.Parallel()

	primary := newFakeStreaming("openai", []string{"gpt-4o"}, nil,
		provider.NewProviderError("openai", provider.ErrorTypeRateLimit, 429, true, "throttled", nil))
	fallback := newFakeStreaming("azure-openai", []string{"gpt-4o"},
		[]provider.StreamChunk{
			{ContentDelta: "served by fallback"},
			{FinishReason: provider.FinishStop},
		}, nil)

	gw, _ := gateway.New(gateway.Config{Providers: []provider.Provider{primary, fallback}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := gw.ChatStream(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}

	var got strings.Builder
	for chunk := range stream {
		got.WriteString(chunk.ContentDelta)
	}
	if got.String() != "served by fallback" {
		t.Errorf("got %q, want fallback's content", got.String())
	}
	if primary.chatStreamCall != 1 || fallback.chatStreamCall != 1 {
		t.Errorf("call counts = (primary=%d, fallback=%d), want (1, 1)",
			primary.chatStreamCall, fallback.chatStreamCall)
	}
}

func TestChatStream_NonRetriablePreStream_NoFallover(t *testing.T) {
	t.Parallel()

	primary := newFakeStreaming("openai", []string{"gpt-4o"}, nil,
		provider.NewProviderError("openai", provider.ErrorTypeAuth, 401, false, "bad key", nil))
	fallback := newFakeStreaming("azure-openai", []string{"gpt-4o"},
		[]provider.StreamChunk{{ContentDelta: "should not be called"}}, nil)

	gw, _ := gateway.New(gateway.Config{Providers: []provider.Provider{primary, fallback}})

	_, err := gw.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, provider.ErrAuthFailed) {
		t.Errorf("errors.Is(err, ErrAuthFailed) = false, want true; got %v", err)
	}
	if fallback.chatStreamCall != 0 {
		t.Errorf("fallback.chatStreamCall = %d, want 0 (non-retriable MUST NOT fail over)", fallback.chatStreamCall)
	}
}

// TestChatStream_AllSyncOnly_SynthesizesInvalidInput pins the
// "streaming-capable filter" branch — if no candidate implements
// StreamingProvider, ChatStream synthesizes a *ProviderError with
// Vendor="gateway" rather than returning a nil channel that would
// deadlock the caller's range loop.
func TestChatStream_AllSyncOnly_SynthesizesInvalidInput(t *testing.T) {
	t.Parallel()

	// fakeProvider from gateway_test.go satisfies Provider but not
	// StreamingProvider — perfect for this branch.
	p := newFake("sync-only-openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{Content: "should not be reached"}, nil
		})

	gw, _ := gateway.New(gateway.Config{Providers: []provider.Provider{p}})

	stream, err := gw.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})

	if stream != nil {
		t.Error("stream != nil — should not allocate channel when no streaming candidate")
	}
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %v", err)
	}
	if pe.Type != provider.ErrorTypeInvalidInput {
		t.Errorf("Type = %q, want %q", pe.Type, provider.ErrorTypeInvalidInput)
	}
	if pe.Vendor() != "gateway" {
		t.Errorf("Vendor = %q, want %q (synthesized router-like error)", pe.Vendor(), "gateway")
	}
}

// TestChatStream_SyncOnlySkipped_StreamingCandidateUsed verifies the
// type-assertion filter: a sync-only primary doesn't consume a
// failover slot — the streaming-capable fallback succeeds without
// being treated as a "fallback" (the primary was never tried).
func TestChatStream_SyncOnlySkipped_StreamingCandidateUsed(t *testing.T) {
	t.Parallel()

	syncOnly := newFake("sync-only", []string{"gpt-4o"}, nil)
	streamer := newFakeStreaming("streamer", []string{"gpt-4o"},
		[]provider.StreamChunk{
			{ContentDelta: "from streamer"},
			{FinishReason: provider.FinishStop},
		}, nil)

	gw, _ := gateway.New(gateway.Config{Providers: []provider.Provider{syncOnly, streamer}})

	stream, err := gw.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}

	var got strings.Builder
	for chunk := range stream {
		got.WriteString(chunk.ContentDelta)
	}
	if got.String() != "from streamer" {
		t.Errorf("got %q", got.String())
	}
	if streamer.chatStreamCall != 1 {
		t.Errorf("streamer.chatStreamCall = %d, want 1", streamer.chatStreamCall)
	}
}

// TestChatStream_RouterFailure_PreStreamError pins the
// "no candidate for model" branch.
func TestChatStream_RouterFailure_PreStreamError(t *testing.T) {
	t.Parallel()

	p := newFakeStreaming("openai", []string{"gpt-4o"}, nil, nil)
	gw, _ := gateway.New(gateway.Config{Providers: []provider.Provider{p}})

	_, err := gw.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "gemini-2.0-pro", // not supported by any provider
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %v", err)
	}
	if pe.Vendor() != "gateway" {
		t.Errorf("Vendor = %q, want %q", pe.Vendor(), "gateway")
	}
}

// TestChatStream_CtxAlreadyCancelled_AbortsImmediately pins the
// "ctx is the only time budget" guarantee at the streaming entry —
// if ctx is dead before the first candidate runs, no ChatStream call
// is made.
func TestChatStream_CtxAlreadyCancelled_AbortsImmediately(t *testing.T) {
	t.Parallel()

	p := newFakeStreaming("openai", []string{"gpt-4o"},
		[]provider.StreamChunk{{ContentDelta: "should not be called"}}, nil)
	gw, _ := gateway.New(gateway.Config{Providers: []provider.Provider{p}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := gw.ChatStream(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true; got %v", err)
	}
	if p.chatStreamCall != 0 {
		t.Errorf("p.chatStreamCall = %d, want 0", p.chatStreamCall)
	}
}
