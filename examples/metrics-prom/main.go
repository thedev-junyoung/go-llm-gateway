// Prometheus-backed metrics example: installs PromRecorder, then exposes
// /metrics on localhost:2112 so a Prometheus scrape picks up
// llm_gateway_requests_total / failovers_total / unknown_model_total /
// attempt_duration_seconds with the labels ADR-006 Q2 pins.
//
// Run:
//
//	OPENAI_API_KEY=sk-... go run ./examples/metrics-prom
//
// Then in another shell:
//
//	curl -s localhost:2112/metrics | grep llm_gateway
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	gateway "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/metrics/promrecorder"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider/openai"
)

func main() { os.Exit(run()) }

func run() int {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		slog.Error("OPENAI_API_KEY is required")
		return 2
	}

	gw, err := gateway.New(gateway.Config{
		Providers: []provider.Provider{openai.New(key)},
		// PromRecorder registers all four metric vectors at construction —
		// see ADR-006 Q2 for the metric set.
		Metrics: promrecorder.New(),
		// KnownModels caps the model label cardinality. ADR-006 Q7's strict
		// default would emit "unknown" for every model — register the ones
		// you actually use so dashboards have meaningful series.
		KnownModels: []string{"gpt-4o-mini", "gpt-4o"},
	})
	if err != nil {
		slog.Error("gateway init failed", "err", err)
		return 1
	}

	// Expose the metrics scrape endpoint on a separate listener — keep it
	// off the application's main HTTP surface so scrape traffic and tenant
	// traffic don't share a goroutine pool.
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		slog.Info("metrics endpoint listening", "addr", "localhost:2112")
		_ = http.ListenAndServe("localhost:2112", nil) //nolint:gosec // demo only
	}()

	// Wait a beat so the listener is up before the first scrape opportunity.
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = gateway.WithRequestID(ctx, "req_prom_demo")

	maxTokens := 64
	resp, err := gw.Chat(ctx, provider.ChatRequest{
		Model:     "gpt-4o-mini",
		System:    "You answer in one short sentence.",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "Why is the sky blue?"}},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		var pe *provider.ProviderError
		if errors.As(err, &pe) {
			slog.Error("chat failed", "vendor", pe.Vendor(), "type", pe.Type)
		} else {
			slog.Error("chat failed", "err", err)
		}
		return 1
	}

	fmt.Println(resp.Content)
	slog.Info("done — scrape localhost:2112/metrics to see counter increments",
		"finish_reason", resp.FinishReason,
		"input_tokens", resp.Usage.InputTokens,
		"output_tokens", resp.Usage.OutputTokens,
	)

	// Keep the process alive briefly so the scrape endpoint can be hit
	// after the chat completes.
	slog.Info("metrics endpoint open for 60s; ctrl-c to exit early")
	time.Sleep(60 * time.Second)
	return 0
}
