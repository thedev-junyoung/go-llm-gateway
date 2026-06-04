// Structured-logging metrics example: installs LogRecorder against a JSON
// slog handler so every attempt and every failover handoff produces a
// machine-parseable line (Loki / CloudWatch Logs Insights / Datadog Logs
// ready). Uses the failover path so the demo also shows the failover
// record alongside attempt records.
//
// Run:
//
//	OPENAI_API_KEY=sk-... go run ./examples/metrics-logging
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	gateway "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/metrics/logrecorder"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider/openai"
)

func main() { os.Exit(run()) }

func run() int {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		slog.Error("OPENAI_API_KEY is required (used by the fallback)")
		return 2
	}

	// JSON handler so the demo output is the same shape an operator would
	// see in production. Source: false to keep the demo output focused on
	// the gateway-emitted attributes.
	jsonLogger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	gw, err := gateway.New(gateway.Config{
		Providers: []provider.Provider{
			&flakyProvider{name: "flaky-primary", models: []string{"gpt-4o-mini"}},
			openai.New(key),
		},
		Metrics:     logrecorder.New(jsonLogger),
		KnownModels: []string{"gpt-4o-mini"},
	})
	if err != nil {
		slog.Error("gateway init failed", "err", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = gateway.WithRequestID(ctx, "req_log_demo")

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

	// Expected output across stdout (3 records on the gateway side):
	//   1. {"level":"WARN", "msg":"gateway attempt", vendor=flaky-primary, outcome=error_overloaded, ...}
	//   2. {"level":"INFO", "msg":"gateway failover", from_vendor=flaky-primary, to_vendor=openai, reason=error_overloaded}
	//   3. {"level":"INFO", "msg":"gateway attempt", vendor=openai, outcome=success, duration_ms=...}
	//
	// (Plus the application's own slog.Info below.)
	slog.Info("served via failover — see JSON lines above for the LogRecorder trace",
		"finish_reason", resp.FinishReason,
		"input_tokens", resp.Usage.InputTokens,
		"output_tokens", resp.Usage.OutputTokens,
	)
	return 0
}

// flakyProvider always reports overloaded so the failover path runs
// without needing two real API keys.
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

func (f *flakyProvider) KeyHash() string { return "example-" + f.name }

func (f *flakyProvider) Chat(_ context.Context, _ provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, provider.NewProviderError(
		f.name, provider.ErrorTypeOverloaded, 503, true,
		"simulated overload for log-recorder demo", nil,
	)
}

var _ provider.Provider = (*flakyProvider)(nil)
