package router_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/router"
)

// fakeProvider is a minimal Provider used to drive table-driven router tests
// without dragging in real adapter packages. Chat is unused — Pick/PickWithFallbacks
// route on SupportsModel alone.
type fakeProvider struct {
	name   string
	models map[string]struct{}
}

var _ provider.Provider = (*fakeProvider)(nil)

func newFake(name string, models ...string) *fakeProvider {
	m := make(map[string]struct{}, len(models))
	for _, model := range models {
		m[model] = struct{}{}
	}
	return &fakeProvider{name: name, models: m}
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) SupportsModel(model string) bool {
	_, ok := f.models[model]
	return ok
}
func (f *fakeProvider) KeyHash() string { return "fake-" + f.name }
func (f *fakeProvider) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, errors.New("fakeProvider.Chat not used in router tests")
}

func TestPick_FirstMatchWins(t *testing.T) {
	t.Parallel()

	openai := newFake("openai", "gpt-4o", "gpt-4o-mini")
	anthropic := newFake("anthropic", "claude-opus-4-7")

	cases := []struct {
		name      string
		providers []provider.Provider
		model     string
		want      string
	}{
		{"single provider matches", []provider.Provider{openai}, "gpt-4o", "openai"},
		{"first provider matches", []provider.Provider{openai, anthropic}, "gpt-4o", "openai"},
		{"second provider matches", []provider.Provider{openai, anthropic}, "claude-opus-4-7", "anthropic"},
		{"both support — first wins (config order)", []provider.Provider{
			newFake("primary", "gpt-4o"),
			newFake("secondary", "gpt-4o"),
		}, "gpt-4o", "primary"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := router.Pick(tc.providers, tc.model)
			if err != nil {
				t.Fatalf("Pick err = %v, want nil", err)
			}
			if got.Name() != tc.want {
				t.Errorf("Pick name = %q, want %q", got.Name(), tc.want)
			}
		})
	}
}

func TestPick_NoMatch_InvalidInputFromGateway(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		providers []provider.Provider
	}{
		{"empty slice", []provider.Provider{}},
		{"nil slice", nil},
		{"providers don't support model", []provider.Provider{
			newFake("openai", "gpt-4o"),
			newFake("anthropic", "claude-opus-4-7"),
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := router.Pick(tc.providers, "gemini-2.0-pro")

			var pe *provider.ProviderError
			if !errors.As(err, &pe) {
				t.Fatalf("err is not *ProviderError: %T", err)
			}
			if pe.Type != provider.ErrorTypeInvalidInput {
				t.Errorf("Type = %q, want %q", pe.Type, provider.ErrorTypeInvalidInput)
			}
			if pe.Vendor() != "gateway" {
				t.Errorf("Vendor() = %q, want %q (routing-layer origin)", pe.Vendor(), "gateway")
			}
			if pe.Retriable {
				t.Error("Retriable = true, want false (unsupported model isn't worth retrying)")
			}
			if !strings.Contains(pe.VendorMessage(), "gemini-2.0-pro") {
				t.Errorf("VendorMessage() = %q, want it to name the model %q", pe.VendorMessage(), "gemini-2.0-pro")
			}
		})
	}
}

func TestPickWithFallbacks_FullChain(t *testing.T) {
	t.Parallel()

	openai := newFake("openai", "gpt-4o")
	azure := newFake("azure-openai", "gpt-4o")
	anthropic := newFake("anthropic", "claude-opus-4-7")

	cases := []struct {
		name          string
		providers     []provider.Provider
		model         string
		wantPrimary   string
		wantFallbacks []string
	}{
		{
			name:          "single supporter — no fallbacks",
			providers:     []provider.Provider{openai, anthropic},
			model:         "claude-opus-4-7",
			wantPrimary:   "anthropic",
			wantFallbacks: nil,
		},
		{
			name:          "multiple supporters preserve config order",
			providers:     []provider.Provider{openai, anthropic, azure},
			model:         "gpt-4o",
			wantPrimary:   "openai",
			wantFallbacks: []string{"azure-openai"},
		},
		{
			name:          "non-supporters excluded from both lists",
			providers:     []provider.Provider{anthropic, openai, azure},
			model:         "gpt-4o",
			wantPrimary:   "openai",
			wantFallbacks: []string{"azure-openai"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			primary, fallbacks, err := router.PickWithFallbacks(tc.providers, tc.model)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if primary.Name() != tc.wantPrimary {
				t.Errorf("primary = %q, want %q", primary.Name(), tc.wantPrimary)
			}
			if len(fallbacks) != len(tc.wantFallbacks) {
				t.Fatalf("fallbacks len = %d (%v), want %d (%v)",
					len(fallbacks), names(fallbacks), len(tc.wantFallbacks), tc.wantFallbacks)
			}
			for i, want := range tc.wantFallbacks {
				if fallbacks[i].Name() != want {
					t.Errorf("fallbacks[%d] = %q, want %q", i, fallbacks[i].Name(), want)
				}
			}
		})
	}
}

func TestPickWithFallbacks_NoMatch_SameErrorAsPick(t *testing.T) {
	t.Parallel()

	openai := newFake("openai", "gpt-4o")
	_, _, err := router.PickWithFallbacks([]provider.Provider{openai}, "gemini-2.0-pro")

	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %T", err)
	}
	if pe.Type != provider.ErrorTypeInvalidInput {
		t.Errorf("Type = %q, want %q", pe.Type, provider.ErrorTypeInvalidInput)
	}
	if pe.Vendor() != "gateway" {
		t.Errorf("Vendor() = %q, want %q", pe.Vendor(), "gateway")
	}
}

func names(ps []provider.Provider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name()
	}
	return out
}
