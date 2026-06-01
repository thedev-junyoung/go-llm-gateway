# go-llm-gateway

> Multi-provider LLM gateway for Go — failover, distributed rate limiting, observability in one library.

**Status:** Pre-release. Building v0.1 (target: 2026-07-06).

---

## Why

Python has [LiteLLM](https://github.com/BerriAI/litellm) (12k+ stars), [Portkey](https://github.com/Portkey-AI/gateway), and [Helicone](https://github.com/Helicone/helicone). The Go ecosystem has nothing comparable.

If you run LLM workloads from a Go service, you typically need:

- **Provider abstraction** — switch OpenAI ↔ Anthropic without rewriting call sites
- **Failover** — when one provider returns 5xx or times out, route to another
- **Distributed rate limits** — protect your API key budget across multiple service instances
- **Observability** — token usage, cost, latency per provider per model

Today, every Go team writes this from scratch. This project ships it as a library.

---

## Quick example

The `Provider` contract and vendor-neutral types are stable as of v0.1.0-rc; the gateway-level composition (`gateway.New`, failover policy, rate limit, metrics) is being built and may shift until the v0.1.0 tag.

```go
import (
    "context"
    "errors"
    "log/slog"
    "os"
    "time"

    gateway "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway"
    "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
    // adapter packages land in follow-up PRs (OpenAI adapter, Anthropic adapter):
    // "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider/openai"
    // "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider/anthropic"
)

func main() {
    gw, err := gateway.New(gateway.Config{
        // Providers slice order IS the priority: for a given model, the
        // first provider whose SupportsModel(model)==true serves the
        // request (see ADR-003). No separate Primary field.
        Providers: []provider.Provider{
            openai.New(os.Getenv("OPENAI_API_KEY")),
            anthropic.New(os.Getenv("ANTHROPIC_API_KEY"),
                anthropic.WithDefaultMaxTokens(4096)), // ADR-002 Q2: Anthropic requires max_tokens
        },

        // Failover trigger policy (ADR-004), RateLimit (Week 4), and
        // Metrics/Logging (Week 5) wiring will arrive as additional
        // non-breaking Config fields. v0.1.0-rc is single-provider
        // passthrough only — adapter Chat errors propagate as-is.
    })
    if err != nil { /* ... */ }

    ctx := gateway.WithRequestID(context.Background(), "req_01HXYZ")

    resp, err := gw.Chat(ctx, provider.ChatRequest{
        Model:    "gpt-4o", // router maps the model id to the active provider
        System:   "You are a terse assistant.",
        Messages: []provider.Message{
            {Role: provider.RoleUser, Content: "Hello"},
        },
    })

    // Router-critical sentinels enable standard errors.Is checks.
    switch {
    case errors.Is(err, provider.ErrRateLimited):
        var pe *provider.ProviderError
        if errors.As(err, &pe) && pe.RetryAfter != nil {
            time.Sleep(*pe.RetryAfter)
        }
    case errors.Is(err, provider.ErrOverloaded), errors.Is(err, provider.ErrTimeout):
        // already retried + failed over by the router; surface to caller
    case err != nil:
        slog.Error("chat failed", "err", err)
        return
    }

    slog.Info("ok", "tokens_out", resp.Usage.OutputTokens, "finish", resp.FinishReason)
    _ = resp.Content
}
```

Stable in v0.1.0-rc:
- `provider.Provider`, `provider.ChatRequest`, `provider.ChatResponse`, `provider.Usage` (ADR-002 / PR #40)
- `provider.FinishReason` constants (`FinishStop`, `FinishLength`, `FinishContentFilter`, `FinishStopSequence`, `FinishToolUse`, `FinishUnknown`)
- `provider.ProviderError` + the four router-critical sentinels (`ErrRateLimited`, `ErrOverloaded`, `ErrAuthFailed`, `ErrTimeout`)
- `pkg/provider/openai`, `pkg/provider/anthropic` adapters (PR #45, #47)
- `pkg/router.Pick`, `pkg/router.PickWithFallbacks` (ADR-003 / PR #53)
- `gateway.Config{Providers}`, `gateway.New`, `Gateway.Chat`, `gateway.ErrNoProviders`
- `gateway.WithRequestID` / `gateway.RequestIDFromContext`

Coming up (still mutating):
- Failover policy fields on `gateway.Config` (ADR-004, Week 3)
- `RedisRateLimit` (Week 4), `PrometheusMetrics` / `slog` wiring (Week 5)

---

## v0.1 scope

- OpenAI + Anthropic providers
- Goroutine-based failover with timeout / 5xx triggers
- Redis-backed distributed rate limit (RPM + TPM, sliding window)
- Prometheus metrics + structured logging (`slog`)

Out of v0.1: semantic cache, streaming, embedding APIs, Gemini/Bedrock/Vertex, HTTP proxy mode. See [docs/design/v0.1-scope.md](docs/design/v0.1-scope.md).

---

## Runnable examples

Self-contained `main` packages under [`examples/`](examples) that compile in CI:

- [`examples/basic`](examples/basic) — one provider, one chat call, sentinel-based error handling
- [`examples/failover`](examples/failover) — a flaky primary always reports "overloaded"; the gateway transparently moves to the OpenAI fallback (ADR-004)

```bash
OPENAI_API_KEY=sk-... go run ./examples/basic
```

---

## Documentation

- [Design — v0.1 scope](docs/design/v0.1-scope.md)
- [Design — Architecture diagram](docs/design/architecture.md)
- [ADR-001 — Why a Go LLM gateway](docs/adr/0001-why-go-llm-gateway.md)
- [ADR-002 — Provider interface design](docs/adr/0002-provider-interface-design.md)
- [ADR-003 — Model routing strategy](docs/adr/0003-model-routing-strategy.md)
- [ADR-004 — Failover trigger + retry policy](docs/adr/0004-failover-trigger-and-retry.md)
- [ADR-005 — Distributed rate limit strategy](docs/adr/0005-distributed-rate-limit.md)
- [Roadmap](docs/roadmap.md)
- [Contributing — development workflow + agent-assisted approach](CONTRIBUTING.md)
- [Agent-driven development rules (internal)](docs/workflow/agent-driven-development.md)

---

## License

MIT (planned).
