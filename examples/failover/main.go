// Failover example: a primary provider that always reports "overloaded"
// (retriable), with a real OpenAI fallback. The first attempt fails, the
// gateway transparently moves to the fallback per ADR-004, and the caller
// sees the OpenAI response.
//
// Run:
//
//	OPENAI_API_KEY=sk-... go run ./examples/failover
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	gateway "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider/openai"
)

func main() { os.Exit(run()) }

// run is split out so deferred cleanup actually fires before exit —
// os.Exit inside main skips defers (gocritic: exitAfterDefer).
func run() int {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		slog.Error("OPENAI_API_KEY is required (used by the fallback)")
		return 2
	}

	// Provider order IS the priority (ADR-003). The flaky one comes first
	// so the gateway has to fall over to OpenAI to satisfy the request.
	gw, err := gateway.New(gateway.Config{
		Providers: []provider.Provider{
			&flakyProvider{name: "flaky-primary", models: []string{"gpt-4o-mini"}},
			openai.New(key),
		},
	})
	if err != nil {
		slog.Error("gateway init failed", "err", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = gateway.WithRequestID(ctx, "req_failover_demo")

	maxTokens := 64
	resp, err := gw.Chat(ctx, provider.ChatRequest{
		Model:     "gpt-4o-mini",
		System:    "You answer in one short sentence.",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "Pick a random color."}},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		var pe *provider.ProviderError
		if errors.As(err, &pe) {
			slog.Error("chat failed after failover", "vendor", pe.Vendor(), "type", pe.Type)
		} else {
			slog.Error("chat failed", "err", err)
		}
		return 1
	}

	// We expect this to be served by OpenAI — the flaky primary always
	// returns ErrOverloaded, so the gateway falls over.
	fmt.Println(resp.Content)
	slog.Info("served via failover", "finish_reason", resp.FinishReason)
	return 0
}

// flakyProvider is an in-example stub that always reports overloaded —
// exercising the failover path without a second real vendor key.
type flakyProvider struct {
	name   string
	models []string
}

func (f *flakyProvider) Name() string { return f.name }

func (f *flakyProvider) SupportsModel(model string) bool {
	for _, m := range f.models {
		if m == model {
			return true
		}
	}
	return false
}

func (f *flakyProvider) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	slog.Info("flaky primary attempted", "vendor", f.name)
	return provider.ChatResponse{}, provider.NewProviderError(
		f.name, provider.ErrorTypeOverloaded, 503, true,
		"simulated overload for failover demo", nil,
	)
}

var _ provider.Provider = (*flakyProvider)(nil)
