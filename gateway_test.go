package gateway_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	gateway "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// fakeProvider exercises Gateway without dragging in the real OpenAI or
// Anthropic adapter packages. ChatFn lets each test inject the response.
type fakeProvider struct {
	name   string
	models map[string]struct{}
	ChatFn func(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error)
}

func newFake(name string, models []string, chat func(context.Context, provider.ChatRequest) (provider.ChatResponse, error)) *fakeProvider {
	m := make(map[string]struct{}, len(models))
	for _, model := range models {
		m[model] = struct{}{}
	}
	return &fakeProvider{name: name, models: m, ChatFn: chat}
}

func (f *fakeProvider) Name() string                    { return f.name }
func (f *fakeProvider) SupportsModel(model string) bool { _, ok := f.models[model]; return ok }
func (f *fakeProvider) KeyHash() string                 { return "fake-" + f.name }
func (f *fakeProvider) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	return f.ChatFn(ctx, req)
}

var _ provider.Provider = (*fakeProvider)(nil)

func TestNew_EmptyProviders_ReturnsErrNoProviders(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  gateway.Config
	}{
		{"nil slice", gateway.Config{Providers: nil}},
		{"empty slice", gateway.Config{Providers: []provider.Provider{}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gw, err := gateway.New(tc.cfg)
			if !errors.Is(err, gateway.ErrNoProviders) {
				t.Errorf("err = %v, want errors.Is == ErrNoProviders", err)
			}
			if gw != nil {
				t.Error("Gateway is not nil on error")
			}
		})
	}
}

func TestNew_NilProviderInSlice_Rejected(t *testing.T) {
	t.Parallel()

	p := newFake("openai", []string{"gpt-4o"}, nil)
	gw, err := gateway.New(gateway.Config{Providers: []provider.Provider{p, nil}})
	if err == nil {
		t.Fatal("err = nil, want non-nil for nil-provider entry")
	}
	if gw != nil {
		t.Error("Gateway is not nil on error")
	}
	if !strings.Contains(err.Error(), "Providers[1]") {
		t.Errorf("err = %q, want it to identify the offending index (Providers[1])", err)
	}
}

func TestNew_WithProviders_Succeeds(t *testing.T) {
	t.Parallel()

	p := newFake("openai", []string{"gpt-4o"}, nil)
	gw, err := gateway.New(gateway.Config{Providers: []provider.Provider{p}})
	if err != nil {
		t.Fatalf("New err = %v, want nil", err)
	}
	if gw == nil {
		t.Fatal("Gateway is nil")
	}
}

func TestChat_RoutesToFirstSupportingProvider(t *testing.T) {
	t.Parallel()

	called := ""
	openai := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			called = "openai"
			return provider.ChatResponse{Content: "from openai", FinishReason: provider.FinishStop}, nil
		})
	anthropic := newFake("anthropic", []string{"claude-opus-4-7"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			called = "anthropic"
			return provider.ChatResponse{Content: "from anthropic", FinishReason: provider.FinishStop}, nil
		})

	gw, err := gateway.New(gateway.Config{Providers: []provider.Provider{openai, anthropic}})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}

	resp, err := gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "claude-opus-4-7",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat err = %v", err)
	}
	if called != "anthropic" {
		t.Errorf("routed to %q, want %q", called, "anthropic")
	}
	if resp.Content != "from anthropic" {
		t.Errorf("Content = %q, want %q", resp.Content, "from anthropic")
	}
}

func TestChat_UnsupportedModel_ReturnsGatewayInvalidInput(t *testing.T) {
	t.Parallel()

	p := newFake("openai", []string{"gpt-4o"}, nil)
	gw, err := gateway.New(gateway.Config{Providers: []provider.Provider{p}})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}

	_, err = gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gemini-2.0-pro",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})

	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %T", err)
	}
	if pe.Type != provider.ErrorTypeInvalidInput {
		t.Errorf("Type = %q, want %q", pe.Type, provider.ErrorTypeInvalidInput)
	}
	if pe.Vendor() != "gateway" {
		t.Errorf("Vendor() = %q, want %q (routing origin)", pe.Vendor(), "gateway")
	}
}

func TestChat_ProviderError_PropagatesVerbatim(t *testing.T) {
	t.Parallel()

	// Single-provider chain: the failover loop exhausts the only candidate
	// and surfaces the (retriable) vendor error. Sentinel survives.
	rateLimited := provider.NewProviderError("openai", provider.ErrorTypeRateLimit, 429, true, "slow down", nil)
	p := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{}, rateLimited
		})

	gw, err := gateway.New(gateway.Config{Providers: []provider.Provider{p}})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}
	_, err = gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})

	if !errors.Is(err, provider.ErrRateLimited) {
		t.Errorf("errors.Is(err, ErrRateLimited) = false, want true (sentinel must survive failover)")
	}
}

func TestChat_Failover_PrimaryRetriable_FallbackSucceeds(t *testing.T) {
	t.Parallel()

	primaryCalls, fallbackCalls := 0, 0
	primary := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			primaryCalls++
			return provider.ChatResponse{}, provider.NewProviderError(
				"openai", provider.ErrorTypeRateLimit, 429, true, "throttled", nil)
		})
	fallback := newFake("azure-openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			fallbackCalls++
			return provider.ChatResponse{Content: "from azure", FinishReason: provider.FinishStop}, nil
		})

	gw, err := gateway.New(gateway.Config{Providers: []provider.Provider{primary, fallback}})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}

	resp, err := gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat err = %v, want nil (fallback should succeed)", err)
	}
	if resp.Content != "from azure" {
		t.Errorf("Content = %q, want %q", resp.Content, "from azure")
	}
	if primaryCalls != 1 || fallbackCalls != 1 {
		t.Errorf("call counts = (primary=%d, fallback=%d), want (1, 1)", primaryCalls, fallbackCalls)
	}
}

func TestChat_Failover_NonRetriable_AbortsImmediately(t *testing.T) {
	t.Parallel()

	primaryCalls, fallbackCalls := 0, 0
	primary := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			primaryCalls++
			return provider.ChatResponse{}, provider.NewProviderError(
				"openai", provider.ErrorTypeAuth, 401, false, "bad key", nil)
		})
	fallback := newFake("azure-openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			fallbackCalls++
			return provider.ChatResponse{Content: "wont reach"}, nil
		})

	gw, err := gateway.New(gateway.Config{Providers: []provider.Provider{primary, fallback}})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}
	_, err = gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})

	if !errors.Is(err, provider.ErrAuthFailed) {
		t.Errorf("errors.Is(err, ErrAuthFailed) = false, want true")
	}
	if primaryCalls != 1 || fallbackCalls != 0 {
		t.Errorf("call counts = (primary=%d, fallback=%d), want (1, 0) — non-retriable MUST NOT fail over", primaryCalls, fallbackCalls)
	}
}

func TestChat_Failover_AllRetriable_ReturnsLastError(t *testing.T) {
	t.Parallel()

	primary := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{}, provider.NewProviderError(
				"openai", provider.ErrorTypeRateLimit, 429, true, "limit", nil)
		})
	fallback := newFake("azure-openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{}, provider.NewProviderError(
				"azure-openai", provider.ErrorTypeOverloaded, 503, true, "down", nil)
		})

	gw, err := gateway.New(gateway.Config{Providers: []provider.Provider{primary, fallback}})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}
	_, err = gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})

	// Last error wins (azure-openai overloaded).
	if !errors.Is(err, provider.ErrOverloaded) {
		t.Errorf("errors.Is(err, ErrOverloaded) = false, want true (azure's error is the last)")
	}
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %v", err)
	}
	if pe.Vendor() != "azure-openai" {
		t.Errorf("vendor of last error = %q, want %q", pe.Vendor(), "azure-openai")
	}
}

func TestChat_Failover_UnknownErrorType_NoFailover(t *testing.T) {
	t.Parallel()

	// Bare error (not *ProviderError) usually means a gateway-side defect.
	// Don't launder it through every vendor — abort.
	bareErr := errors.New("something exploded internally")
	primaryCalls, fallbackCalls := 0, 0
	primary := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			primaryCalls++
			return provider.ChatResponse{}, bareErr
		})
	fallback := newFake("azure-openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			fallbackCalls++
			return provider.ChatResponse{}, nil
		})

	gw, err := gateway.New(gateway.Config{Providers: []provider.Provider{primary, fallback}})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}
	_, err = gw.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})

	if !errors.Is(err, bareErr) {
		t.Errorf("err mismatch: got %v, want bareErr to propagate verbatim", err)
	}
	if primaryCalls != 1 || fallbackCalls != 0 {
		t.Errorf("call counts = (primary=%d, fallback=%d), want (1, 0) — unknown error MUST NOT fail over", primaryCalls, fallbackCalls)
	}
}

func TestChat_Failover_CtxCancelMidLoop_DualWrap(t *testing.T) {
	t.Parallel()

	// Primary returns retriable; the closure also cancels ctx so the loop's
	// next iteration aborts before fallback is tried. Caller must be able to
	// errors.Is for BOTH the ctx cause AND the vendor sentinel (Go 1.20+ %w).
	ctx, cancel := context.WithCancel(context.Background())
	primary := newFake("openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			cancel()
			return provider.ChatResponse{}, provider.NewProviderError(
				"openai", provider.ErrorTypeRateLimit, 429, true, "throttled", nil)
		})
	fallbackCalls := 0
	fallback := newFake("azure-openai", []string{"gpt-4o"},
		func(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
			fallbackCalls++
			return provider.ChatResponse{}, nil
		})

	gw, err := gateway.New(gateway.Config{Providers: []provider.Provider{primary, fallback}})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}
	_, err = gw.Chat(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})

	if !errors.Is(err, context.Canceled) {
		t.Error("errors.Is(err, context.Canceled) = false, want true")
	}
	if !errors.Is(err, provider.ErrRateLimited) {
		t.Error("errors.Is(err, ErrRateLimited) = false, want true (last vendor error preserved)")
	}
	if fallbackCalls != 0 {
		t.Errorf("fallbackCalls = %d, want 0 (ctx cancelled before fallback try)", fallbackCalls)
	}
}
