package gateway_test

import (
	"context"
	"testing"

	gateway "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// noopProvider returns a stub success without touching the network. Used in
// benchmarks to measure GATEWAY overhead — failover orchestration, attempt
// info construction, recorder fan-out — independent of vendor latency.
type noopProvider struct {
	name   string
	models map[string]struct{}
	fail   bool // when true, return retriable error to force failover
}

func newNoop(name string, models []string, fail bool) *noopProvider {
	m := make(map[string]struct{}, len(models))
	for _, model := range models {
		m[model] = struct{}{}
	}
	return &noopProvider{name: name, models: m, fail: fail}
}

func (n *noopProvider) Name() string                    { return n.name }
func (n *noopProvider) SupportsModel(model string) bool { _, ok := n.models[model]; return ok }
func (n *noopProvider) KeyHash() string                 { return "bench-" + n.name }

func (n *noopProvider) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	if n.fail {
		return provider.ChatResponse{}, provider.NewProviderError(
			n.name, provider.ErrorTypeOverloaded, 503, true,
			"benchmark forced failover", nil,
		)
	}
	return provider.ChatResponse{
		Content:      "ok",
		FinishReason: provider.FinishStop,
		Usage:        provider.Usage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

var _ provider.Provider = (*noopProvider)(nil)

// BenchmarkChat_SingleProvider measures the gateway floor — one provider,
// no failover, no rate limit, no recorder. Anything above this number is
// either failover orchestration or recorder fan-out.
func BenchmarkChat_SingleProvider(b *testing.B) {
	gw, err := gateway.New(gateway.Config{
		Providers: []provider.Provider{newNoop("openai", []string{"gpt-4o"}, false)},
	})
	if err != nil {
		b.Fatalf("New err = %v", err)
	}
	req := provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	}
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := gw.Chat(ctx, req)
		if err != nil {
			b.Fatalf("Chat err = %v", err)
		}
	}
}

// BenchmarkChat_FailoverOverhead is the v0.1 DoD probe: failover from a
// flaky primary to a successful fallback. Subtract the SingleProvider
// number to isolate the gateway's failover orchestration cost — DoD
// target is < 1ms (excluding vendor call latency, which is zero here).
func BenchmarkChat_FailoverOverhead(b *testing.B) {
	gw, err := gateway.New(gateway.Config{
		Providers: []provider.Provider{
			newNoop("primary", []string{"gpt-4o"}, true), // forces failover
			newNoop("fallback", []string{"gpt-4o"}, false),
		},
	})
	if err != nil {
		b.Fatalf("New err = %v", err)
	}
	req := provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	}
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := gw.Chat(ctx, req)
		if err != nil {
			b.Fatalf("Chat err = %v", err)
		}
	}
}
