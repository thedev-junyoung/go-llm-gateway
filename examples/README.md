# examples

Runnable demonstrations of `go-llm-gateway`. Each subdirectory is a
self-contained `main` package — `go run ./examples/<name>` and you're in.

| Example | What it shows | Required env |
|---|---|---|
| [`basic`](basic) | One provider, one Chat call, sentinel-based error handling | `OPENAI_API_KEY` |
| [`failover`](failover) | A flaky primary always returns "overloaded"; the gateway transparently falls over to the real OpenAI fallback (ADR-004) | `OPENAI_API_KEY` |

## Run

```bash
export OPENAI_API_KEY=sk-...

go run ./examples/basic
go run ./examples/failover
```

`go build ./examples/...` compiles every example without API keys, so the
examples are kept honest by CI.

## What these examples are NOT

- **No mock for OpenAI** — `basic` makes a real call. If you want offline
  development, see the upcoming `go-vcr` fixture work (Week 1-2 leftover).
- **No rate-limit / metrics wiring** — those land with ADR-005 / ADR-006
  (Week 4-5).
- **No Docker / k8s setup** — Week 6 release work.
