# ADR-008 — OpenTelemetry Tracing Integration

- **Status:** Proposed *(agent-generated draft, pending maintainer review)*
- **Date:** 2026-06-08
- **Author:** Junyoung
- **비고:** 본 draft 의 결정/근거는 에이전트가 사용자 요청으로 작성. 각 Q 의 `Maintainer note` 줄을 본인 voice 로 채우기 전엔 `Status` 를 `Accepted` 로 바꾸지 말 것.

---

## Context

ADR-006 은 Prometheus metrics + slog structured logging 을 observability 의 두 기둥으로 land 했다. ADR-006 Q6 는 OTel tracing 을 명시적으로 "v0.2 후보" 로 deferred 하면서 두 가지 조건을 남겼다:

1. `MetricRecorder` 인터페이스가 OTel adapter 의 확장점 역할 (ctx 를 hook 시그니처에 포함)
2. OTel 도입 시 **별 ADR 에서 ctx propagation 룰 재검토** 필요

두 조건 모두 v0.1 land 시점(ADR-006, PR #75)에 충족됐다. `MetricRecorder.OnAttempt` 와 `OnFailover` 가 모두 `ctx context.Context` 를 첫 인자로 받으며, ctx propagation 룰 재검토가 본 ADR 의 Q4/Q5 에서 이루어진다. v0.2 (streaming, ADR-007) 이 main 에 land 된 지금이 세 번째 기둥인 **분산 추적** 을 결정하는 시점이다.

**왜 tracing 이 필요한가:**

- Prometheus 는 "얼마나 / 얼마나 자주 / 얼마나 빠르게" 를 답한다.
- slog 는 "무슨 이벤트가 일어났나" 를 답한다.
- Trace 만이 "왜 이 요청이 800ms 걸렸나" 를 맥락 있게 답한다.

failover 경로는 attempt N이 실패하고 attempt N+1 이 성공하는 복수 hop 이다. 스트리밍은 수십 초를 걸친 수명이다. 두 경우 모두 p99 histogram 이 "느리다" 를 알려줄 수 있지만, **어느 attempt 가, 어느 vendor 의 어떤 단계에서, 얼마를 잡아먹었는지** 를 단일 view 로 보여주는 것은 trace 뿐이다. caller 가 이미 OTel-instrumented 서비스라면 gateway 호출이 span tree 에 구멍으로 남는다.

영향 모듈:

- `pkg/tracing` (신규) — `TracingHook` 인터페이스 + `NoOpTracingHook` 기본 구현
- `pkg/tracing/oteltracing` (신규 sub-package) — OTel API-only 구현체. 코어에 OTel 의존성 없음.
- `pkg/gateway` (수정) — `Config.Tracing TracingHook` 필드 추가, `Chat` / `ChatStream` 호출 경계에 hook 삽입
- `pkg/metrics` (확장) — Prometheus exemplar helper (Q6). `MetricRecorder` 인터페이스 변경 없음.

추가 제약:

- ADR-002 의 "외부 의존성 최소화" 원칙 — OTel 전체 SDK (~5MB) 를 코어에 강제 도입 금지
- ADR-006 Q6 의 `AsyncWrapper + OTel ctx staleness` 미해결 문제 본 ADR 에서 land
- ADR-007 의 streaming 경로도 tracing 범위에 포함 (ChatStream span + first-token event)

---

## Decision

### Q1. OTel 범위 — Traces / Metrics bridge / Logs bridge 중 무엇을

- **Decision:** **Traces 전용 (이 ADR). Metrics bridge 와 Logs bridge 는 별 ADR.**
- **Agent reasoning:** 세 가지 범위 비교:
  - **A. Traces 전용** ✅ — ADR-006 이 "v0.2 후보" 로 남긴 항목이 OTel tracing 이다. Metrics 와 Logs 는 각각 `MetricRecorder` 인터페이스와 slog 로 이미 안정 land 됐다. 범위를 tracing 에 한정하면 단일 ADR 이 답할 Q 의 수가 유한해진다.
  - **B. Traces + Metrics bridge** — `MetricRecorder` 의 OTel 구현 (`oteltracing.MetricRecorder`) 을 같은 ADR 에. 매력적이나 두 독립 관심사의 결정이 섞여 ADR 가 비대해짐. Metrics bridge 의 cardinality 맵핑 (ADR-006 Q7 의 `KnownModels` whitelist 재현) 이 별도 결정이 필요한 complexity 를 가짐.
  - **C. Full OTel (traces + metrics + logs)** — OTel Logs API 는 stable 하지만 ADR-006 Q6 의 Alt 8 에서 "v0.2 후보, 별 ADR-009 에서 일괄" 로 결론냈다. 한 ADR 에 세 기둥 동시 결정은 ADR 를 설계 문서가 아닌 feature spec 으로 만든다.
- **왜 이 결정이 정당한가:** ADR 는 단일 관심사 결정 단위. "tracing 을 어떻게" 와 "metrics backend 를 OTel 로 교체할지" 는 다른 질문이다. 본 ADR 가 traces 를 결정하면 후속 ADR 가 metrics bridge / logs bridge 를 각각 결정한다. 이 순서가 ADR-006 의 deferred list 와도 일치한다.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q2. 의존성 모델 — API-only vs 전체 SDK, 서브 패키지 분리 여부

- **Decision:** **코어 zero OTel 의존성 + `pkg/tracing/oteltracing` 별 sub-package (API-only 의존).**
  ```
  go-llm-gateway/
  ├── pkg/tracing/           # TracingHook interface + NoOpTracingHook (zero external deps)
  │   └── oteltracing/       # go.opentelemetry.io/otel API-only 구현 (선택적 import)
  └── pkg/gateway/           # Config.Tracing TracingHook 사용 (tracing 인터페이스만 의존)
  ```
  - `pkg/tracing` 는 stdlib 만 — `go.opentelemetry.io/*` import 없음.
  - `pkg/tracing/oteltracing` 는 `go.opentelemetry.io/otel` (trace API, ~300KB) 만. 전체 SDK (exporter, propagator) 는 caller 의 main 에서.
- **Agent reasoning:**
  - **A. 코어에 OTel SDK 전체** — OTel 안 쓰는 사용자가 ~5MB 의존성 강제. ADR-002 의 "minimal external" 원칙 위반. OTel 안 쓰는 사용자에게 dead weight.
  - **B. API-only 코어 의존** — API 패키지 (~300KB) 만 코어 `go.mod` 에. SDK / exporter 는 caller 가 inject. 매력적이나, 여전히 OTel 안 쓰는 사용자 (Prometheus-only) 가 API 패키지를 indirect 로 끌어옴.
  - **C. 코어 zero 의존 + 별 sub-package** ✅ — `TracingHook` 인터페이스를 stdlib-only 로 정의. OTel 사용자만 `oteltracing` sub-package 를 import. ADR-005 Q1 의 `RateLimiter` 인터페이스 + `RedisBackend` 별 구현 패턴과 동일.
  - **D. 별 Go module (`go-llm-gateway/oteltracing`)** — sub-package 가 아닌 독립 Go module. Go module 분리는 major version 분리나 release cadence 분리가 필요할 때 가치 있음. 같은 repo 에서 별 module 은 `go.work` 없이는 로컬 개발이 번거로움. 현 시점엔 sub-package 로 충분.
- **왜 이 결정이 정당한가:** `RateLimiter` ↔ `RedisBackend` / `MetricRecorder` ↔ `PromRecorder` 의 패턴이 이미 이 프로젝트의 확장 convention 이다. tracing 도 동일한 패턴을 따르면 사용자의 mental model 이 일관된다.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q3. Span 계층 — gateway.Chat, attempt, ChatStream

- **Decision:** **2-level hierarchy: gateway span (root) + attempt child spans. ChatStream 은 streaming event 를 root span 에 span events 로 기록.**
  ```
  gateway.Chat (root span)
  ├── attributes: model, candidate_count, final_vendor, outcome
  ├── attempt[0] (child span)
  │   └── attributes: vendor, model, attempt_num, outcome, duration_ms
  ├── attempt[1] (child span, failover case)
  │   └── attributes: vendor, model, attempt_num, outcome, duration_ms
  └── (span ends when Chat returns)

  gateway.ChatStream (root span)
  ├── attributes: model, candidate_count, final_vendor
  ├── attempt[0] (child span)  ← pre-stream phase only (ADR-007 Q4)
  ├── span event: "first_token" (timestamp, delta_len)
  ├── span event: "stream_end" (outcome, total_chunks, total_ms)
  └── (span ends when stream channel closes)
  ```
- **Agent reasoning:**
  - **A. 단일 gateway span 만** — 빠른 구현, 그러나 attempt 분해가 span attributes 에 flat 하게 들어감 (attempt_0_vendor, attempt_0_outcome ...). UI 가 timeline 으로 표현 불가. failover 가 있는 경우 p99 attribution 이 불가.
  - **B. gateway span + attempt child spans** ✅ — 자연스러운 tree. Jaeger / Tempo 가 child span 을 waterfall 로 렌더링. attempt 별 duration 이 독립적으로 보임. failover 경로에서 "attempt 0 = 200ms fail, attempt 1 = 150ms success" 가 즉시 visible.
  - **C. provider 내부까지 (HTTP request span)** — vendor HTTP call 내부에 span 을 넣으려면 provider 가 `TracingHook` 을 받아야 함. provider 는 현재 tracing 무지. provider 인터페이스 변경 = ADR-002 breaking change 위험. vendor 는 W3C traceparent 를 의미 있게 전파하지도 않음.
  - **ChatStream 의 child span 범위:** ADR-007 Q4 의 "pre-stream failover only" 원칙에 따라, attempt child span 은 pre-stream phase (`ChatStream` 호출 → 첫 chunk channel 획득) 에만. 첫 chunk 이후는 streaming session 전체가 root span 의 span events 로 기록. mid-stream 을 per-chunk span 으로 쪼개는 건 trace backend 에 수천 event 를 밀어넣는 cardinality 폭발.
- **왜 이 결정이 정당한가:** 2-level hierarchy 가 운영자의 가장 흔한 질문 ("어느 vendor 가 느렸나") 에 직접 답한다. provider 내부까지 파고드는 건 이 라이브러리의 "gateway-layer abstraction" 정체성 바깥.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q4. Trace context propagation — caller ctx → downstream

- **Decision:** **OTel `trace.SpanFromContext(ctx)` 로 caller 의 span 을 parent 로 사용. vendor HTTP request 에 W3C traceparent 헤더는 inject 하지 않음.**
- **Agent reasoning:**
  - **A. Caller ctx 에서 parent span 추출만** ✅ — `otel.SetTextMapPropagator` / `propagation.TraceContext` 를 coercion 없이 사용. caller 의 기존 span tree 에 gateway span 이 자연히 child 로 붙음.
  - **B. Vendor HTTP request 에 W3C 헤더 inject** — `otelhttp` transport 를 provider HTTP client 에 wrapping. 두 가지 문제: (1) provider HTTP client 는 현재 tracing-agnostic — provider 가 `*http.Client` 를 외부에서 주입받지 않는 구현이면 interceptor 삽입이 private field 수정 필요. (2) LLM vendor (OpenAI, Anthropic, Gemini) 는 traceparent 헤더를 의미 있게 처리하지 않음 — trace 가 vendor 서버 내부를 따라가지 않으므로, 주입해도 추가 가시성 0.
  - **C. Baggage 전파 (user_id, request_id 등)** — OTel baggage 로 metadata 를 vendor HTTP 헤더에 흘려보냄. LLM vendor 에게 baggage 를 전달할 이유 없음 (vendor 는 읽지도 처리하지도 않음). baggage 는 caller 의 서비스 내부 (gateway → caller 의 다른 서비스) 에만 의미 있고, 그건 caller 가 직접 관리한다.
- **왜 이 결정이 정당한가:** "caller 의 span tree 에 붙는다" 는 것만으로 tracing 의 핵심 가치 (gateway 호출이 upstream trace 에 구멍으로 남지 않는다) 가 달성된다. vendor HTTP 주입은 추가 복잡도만 있고 가시성 이득이 없다.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q5. AsyncWrapper ctx staleness — OTel 과의 조합 해결

- **Decision:** **`TracingHook` 은 `AsyncWrapper` 우회 — 트레이싱은 항상 동기 호출. AsyncWrapper 의 godoc 에 "OTel TracingHook 사용 시 AsyncWrapper 를 MetricRecorder 에만 적용할 것" 명시.**
- **Agent reasoning:**
  ADR-006 Q6 Sub-decision B 가 이미 "AsyncWrapper + OTel 조합 사용 금지" 를 결정했다. 문제는 그 금지를 **어떻게 구조로 강제하느냐** 다.

  세 가지 옵션:
  - **A. `TracingHook` 을 `MetricRecorder` 와 별 인터페이스로 분리** ✅ — `Config.Metrics MetricRecorder` 와 `Config.Tracing TracingHook` 이 독립 필드. `MetricRecorder` 는 AsyncWrapper 로 감쌀 수 있고, `TracingHook` 은 gateway 가 항상 동기 직접 호출. AsyncWrapper 가 `TracingHook` 을 받을 필요 자체가 없어짐 — 타입이 다르므로 런타임 혼입 불가.
  - **B. `AttemptInfo` 에 trace snapshot 포함** — `TraceID` / `SpanID` 를 `AttemptInfo` 에 추가해 AsyncWrapper 가 channel 에 snapshot 을 전달. ADR-006 Q6 Sub-decision A 가 이미 reject 했다 (YAGNI — 실제 span 추출 로직 없으면 빈 string 두 개가 dead weight).
  - **C. ctx-preserving AsyncWrapper variant** — channel 에 ctx 대신 ctx snapshot (span 포함) 저장. 구현 복잡. span 은 ref-type 이라 snapshot 이 아닌 pointer — cancelled ctx 에서 꺼낸 span 은 이미 ended 된 상태일 수 있음.
  - **왜 A가 정당한가:** 타입 시스템이 "MetricRecorder 에는 AsyncWrapper OK, TracingHook 은 직접 호출" 을 강제한다. span Start/End 는 in-memory lock 동작이므로 p99 tail latency 영향 < 1µs (sub-microsecond lock, heap alloc 없음). "동기 호출이 느리다" 는 우려 자체가 해소됨.
- **`TracingHook` 인터페이스 (초안):**
  ```go
  // pkg/tracing/hook.go

  // TracingHook receives lifecycle events from gateway.Chat and
  // gateway.ChatStream. All methods are called synchronously on the
  // hot path — implementations must not block. OTel span operations
  // are sub-microsecond in-memory; slower backends must buffer
  // internally (NOT via AsyncWrapper — see AsyncWrapper godoc).
  type TracingHook interface {
      // OnChatStart is called at the entry of gateway.Chat or
      // gateway.ChatStream before any provider is contacted.
      // Returns a context that carries the root span; the caller
      // must pass this ctx to OnChatEnd and OnAttemptStart.
      OnChatStart(ctx context.Context, model string) context.Context

      // OnChatEnd is called just before gateway.Chat returns or
      // just before the ChatStream producer goroutine exits.
      OnChatEnd(ctx context.Context, outcome string, err error)

      // OnAttemptStart is called before each provider attempt
      // (pre-stream phase for ChatStream). Returns a context
      // carrying the attempt child span.
      OnAttemptStart(ctx context.Context, vendor, model string, attemptNum int) context.Context

      // OnAttemptEnd is called after each attempt completes
      // (success or failure).
      OnAttemptEnd(ctx context.Context, outcome string, err error)

      // OnFirstToken is called when the first ContentDelta!=""
      // chunk arrives from ChatStream. It is a span event on the
      // root span (not a child span — see Q3).
      OnFirstToken(ctx context.Context)

      // OnStreamEnd is called when the ChatStream producer goroutine
      // closes the channel. failurePhase is non-empty only on error
      // outcomes; values: "pre_stream", "mid_stream", "ctx_cancel".
      OnStreamEnd(ctx context.Context, outcome string, totalChunks int, failurePhase string)
  }

  // NoOpTracingHook discards all events. Gateway default when
  // Config.Tracing is nil.
  type NoOpTracingHook struct{}

  func (NoOpTracingHook) OnChatStart(ctx context.Context, _ string) context.Context {
      return ctx
  }
  func (NoOpTracingHook) OnChatEnd(context.Context, string, error)           {}
  func (NoOpTracingHook) OnAttemptStart(ctx context.Context, _, _ string, _ int) context.Context {
      return ctx
  }
  func (NoOpTracingHook) OnAttemptEnd(context.Context, string, error)        {}
  func (NoOpTracingHook) OnFirstToken(context.Context)                            {}
  func (NoOpTracingHook) OnStreamEnd(context.Context, string, int, string)        {}
  ```
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q6. Prometheus exemplar — OTel trace ID 연결

- **Decision:** **`pkg/metrics` 에 `ExemplarRecorder` opt-in 인터페이스 추가 + `pkg/tracing` 에 `TraceIDExtractor` opt-in 인터페이스 추가. Gateway 가 두 인터페이스를 type-assert 해 exemplar 를 orchestrate. `pkg/tracing` 과 `pkg/metrics` 사이 직접 의존 없음.**
- **Agent reasoning:**
  Prometheus 2.x 는 histogram observation 에 exemplar (trace_id 포함 label 셋) 를 붙일 수 있다. 이를 통해 "p99 spike 시점의 실제 trace" 로 직접 jump 가 가능하다 (Grafana → Tempo 연결).

  두 가지 통합 방법:
  - **A. Push-callback (`ExemplarTarget`) 패턴** — `oteltracing.OtelTracingHook` 가 `OnAttemptEnd` 에서 `ExemplarTarget.RecordExemplar(traceID)` 를 call → `PromRecorder` 가 내부에 traceID 를 저장 → 다음 `OnAttempt` 호출 시 첨부. **Reject**: concurrent request 환경에서 Request A 의 `RecordExemplar` 가 Request B 의 `OnAttempt` 직전에 도착하면 traceID 가 교차 오염된다. goroutine ID 는 Go 공개 API 에 없고, ctx 는 immutable 이라 `RecordExemplar` 호출 시점 에 ctx 에 저장 불가. 구조적 data race — 해결할 수 없는 설계 결함.
  - **B. Gateway orchestration + `TraceIDExtractor`** ✅ — `pkg/tracing` 에 optional 인터페이스 하나 추가:
    ```go
    // pkg/tracing/hook.go (추가)

    // TraceIDExtractor is an optional interface for TracingHook
    // implementations that can surface the active OTel trace ID.
    // Gateway type-asserts; if the hook does not implement this,
    // exemplar attachment is silently skipped.
    type TraceIDExtractor interface {
        ExtractTraceID(ctx context.Context) string // 32-char hex or ""
    }
    ```
    `OtelTracingHook` 가 구현:
    ```go
    func (h *OtelTracingHook) ExtractTraceID(ctx context.Context) string {
        return trace.SpanFromContext(ctx).SpanContext().TraceID().String()
    }
    ```
    `pkg/metrics` 에 `ExemplarRecorder` opt-in 인터페이스:
    ```go
    // pkg/metrics/exemplar.go

    type ExemplarRecorder interface {
        MetricRecorder
        ObserveAttemptWithExemplar(ctx context.Context, info provider.AttemptInfo, traceID string)
    }
    ```
    Gateway 가 attempt end 직후 orchestrate:
    ```go
    g.tracing.OnAttemptEnd(attemptCtx, outcome, err)
    // Exemplar bridge: stateless, no shared mutable state.
    if te, ok := g.tracing.(tracing.TraceIDExtractor); ok {
        if er, ok := g.metrics.(metrics.ExemplarRecorder); ok {
            er.ObserveAttemptWithExemplar(attemptCtx, info, te.ExtractTraceID(attemptCtx))
        }
    }
    ```
    - traceID 추출과 metrics 기록이 같은 goroutine, 같은 시점 — per-request 격리 완전 보장.
    - `ExemplarTarget` 인터페이스 불필요. `oteltracing` 이 `pkg/metrics` 타입을 알 필요 없음.
    - `pkg/tracing` 과 `pkg/metrics` 는 상호 독립 유지.
- **왜 이 결정이 정당한가:** gateway 는 이미 `g.tracing` 과 `g.metrics` 두 필드를 모두 소유하고 있다. 두 optional interface 를 조합하는 orchestrator 역할이 자연스럽다. stateful bridge 없이 race-free. `StreamingMetricRecorder` 의 type-assertion 선례와 일관.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q7. Span attribute cardinality

- **Decision:** **ADR-006 Q7 의 model whitelist 를 span attribute 에도 동일 적용. `TracingHook` 에 model 이름 정규화 책임은 없음 — gateway 가 hook 호출 전 정규화.**
- **Agent reasoning:**
  - Span attribute 의 `model` 값이 무제한이면 trace backend (Tempo / Jaeger) 의 인덱스가 폭발. Prometheus cardinality 문제와 동일한 구조.
  - ADR-006 Q7 결정: gateway 가 `KnownModels` whitelist 로 model 값을 정규화 (미등록 = `"unknown"`). 같은 정규화 로직이 `gateway.Chat` 진입 시점에 이미 실행된다.
  - `TracingHook.OnChatStart(ctx, model)` 에 전달되는 `model` 은 이미 gateway 가 정규화한 값. hook 구현체가 다시 정규화할 필요 없음.
  - Span 에 추가할 attribute: `llm.vendor`, `llm.model`, `llm.attempt_num`, `llm.outcome`. [OTel LLM semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/) 이 정의하는 `gen_ai.*` 네이밍과 ADR-006 의 `llm_gateway_*` 네이밍을 혼용하지 않기 위해 **이 프로젝트는 `llm.*` prefix 를 일관 사용** (OTel semconv 는 draft 상태이므로 stable 확정 시 별 ADR 에서 rename 결정).
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

---

### Synthesis — 구조 & Pseudocode

의존성 방향 (기존 ADR 과 일관):

```
pkg/types  (leaf)
    ↑
pkg/provider  (Provider, StreamingProvider, StreamChunk)
    ↑                         ↑
pkg/metrics               pkg/tracing           ← 독립 브랜치, stdlib only
(MetricRecorder,          (TracingHook,
 ExemplarRecorder)         NoOpTracingHook,
                           TraceIDExtractor)
                               ↑
                    pkg/tracing/oteltracing     ← otel API-only
                               ↑
pkg/gateway  ←────────────────┘
(imports both pkg/metrics and pkg/tracing; orchestrates exemplar bridge)
```

**`pkg/tracing/oteltracing/hook.go` 스케치:**

```go
package oteltracing

import (
    "context"

    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/codes"
    "go.opentelemetry.io/otel/trace"

    "github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/tracing"
)

const tracerName = "go-llm-gateway"

// OtelTracingHook implements tracing.TracingHook using the OTel trace API.
// The caller must configure a TracerProvider (e.g. via otel.SetTracerProvider)
// before using this hook. If no provider is configured the global NoopProvider
// is used — spans are created but immediately discarded.
//
// OtelTracingHook is safe for concurrent use.
type OtelTracingHook struct {
    tracer trace.Tracer
}

// New returns an OtelTracingHook using the global OTel TracerProvider.
func New() *OtelTracingHook {
    return &OtelTracingHook{tracer: otel.Tracer(tracerName)}
}

// Ensure compile-time interface satisfaction.
var _ tracing.TracingHook = (*OtelTracingHook)(nil)

func (h *OtelTracingHook) OnChatStart(ctx context.Context, model string) context.Context {
    ctx, _ = h.tracer.Start(ctx, "llm_gateway.chat",
        trace.WithAttributes(attribute.String("llm.model", model)),
    )
    return ctx
}

func (h *OtelTracingHook) OnChatEnd(ctx context.Context, outcome string, err error) {
    span := trace.SpanFromContext(ctx)
    span.SetAttributes(attribute.String("llm.outcome", outcome))
    if err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, err.Error())
    } else {
        span.SetStatus(codes.Ok, "")
    }
    span.End()
}

func (h *OtelTracingHook) OnAttemptStart(ctx context.Context, vendor, model string, n int) context.Context {
    ctx, _ = h.tracer.Start(ctx, "llm_gateway.attempt",
        trace.WithAttributes(
            attribute.String("llm.vendor", vendor),
            attribute.String("llm.model", model),
            attribute.Int("llm.attempt_num", n),
        ),
    )
    return ctx
}

func (h *OtelTracingHook) OnAttemptEnd(ctx context.Context, outcome string, err error) {
    span := trace.SpanFromContext(ctx)
    span.SetAttributes(attribute.String("llm.outcome", outcome))
    if err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, err.Error())
    } else {
        span.SetStatus(codes.Ok, "")
    }
    span.End()
}

// ExtractTraceID implements tracing.TraceIDExtractor. Gateway calls this
// after OnAttemptEnd to feed the trace ID into ExemplarRecorder.
// Returns "" when no active span is found (NoopSpan or unset provider).
func (h *OtelTracingHook) ExtractTraceID(ctx context.Context) string {
    sc := trace.SpanFromContext(ctx).SpanContext()
    if !sc.IsValid() {
        return ""
    }
    return sc.TraceID().String()
}

func (h *OtelTracingHook) OnFirstToken(ctx context.Context) {
    trace.SpanFromContext(ctx).AddEvent("first_token")
}

func (h *OtelTracingHook) OnStreamEnd(ctx context.Context, outcome string, totalChunks int, failurePhase string) {
    attrs := []attribute.KeyValue{
        attribute.String("llm.outcome", outcome),
        attribute.Int("llm.total_chunks", totalChunks),
    }
    if failurePhase != "" {
        attrs = append(attrs, attribute.String("llm.failure_phase", failurePhase))
    }
    trace.SpanFromContext(ctx).AddEvent("stream_end", trace.WithAttributes(attrs...))
}
```

**`gateway.Chat` 통합 스케치 (변경 부분만):**

```go
func (g *Gateway) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
    // tracing: root span
    ctx = g.tracing.OnChatStart(ctx, req.Model)
    var chatOutcome string
    var chatErr error
    defer func() { g.tracing.OnChatEnd(ctx, chatOutcome, chatErr) }()

    // ... existing failover loop ...
    for i, p := range candidates {
        // tracing: attempt child span (scoped to runAttempt to avoid ctx confusion)
        attemptCtx := g.tracing.OnAttemptStart(ctx, p.Name(), req.Model, i)
        resp, err := p.Chat(attemptCtx, req)
        outcome := outcomeStr(err)
        g.tracing.OnAttemptEnd(attemptCtx, outcome, err)

        // Exemplar bridge: race-free — same goroutine, after span.End().
        if te, ok := g.tracing.(tracing.TraceIDExtractor); ok {
            if er, ok := g.metrics.(metrics.ExemplarRecorder); ok {
                er.ObserveAttemptWithExemplar(attemptCtx, attemptInfo, te.ExtractTraceID(attemptCtx))
            }
        }

        if err == nil {
            chatOutcome = "success"
            return resp, nil
        }
        // ... failover logic ...
    }
    chatOutcome = "error"
    chatErr = lastErr
    return provider.ChatResponse{}, lastErr
}
```

---

## Consequences

### Positive

- **OTel TracerProvider 가 configured 된 환경에서**: caller 의 기존 OTel span tree 에 gateway 호출이 child 로 붙음 — trace 에 구멍이 없어짐. failover 경로가 span waterfall 로 즉시 가시화 — "어느 vendor 가 얼마를 잡아먹었나" 를 trace UI 에서 직접 확인. Prometheus exemplar 연결로 p99 spike → 실제 trace 의 Grafana drilldown 가능.
- ADR-006 Q6 의 ctx staleness 미해결 문제가 `TracingHook` 분리로 구조적으로 해결됨.
- OTel 안 쓰는 사용자 영향 제로 (`pkg/tracing` 는 zero external deps; `oteltracing` sub-package 는 선택 import). `otel.SetTracerProvider` 미설정 시 global `NoopProvider` 로 silent no-op.

### Negative

- `gateway.Chat` / `gateway.ChatStream` 에 hook 삽입 → hot path 에 함수 호출 2-6개 추가. 실 SDK `TracerProvider` 에서 `span.Start()` 는 heap alloc (~200B) + `time.Now()` 2회 + goroutine-safe ID 생성을 포함해 1–10 µs 범위. failover 3 attempt 조합 시 30–60 µs 추가 가능. `NoopProvider` 기준 sub-µs 주장은 기준이 다름 — 실측 benchmark 필요 (Open Questions 참고).
- `TracingHook` 인터페이스가 `OnAttemptStart` 가 ctx 를 반환 — 기존 `MetricRecorder.OnAttempt(ctx, info)` 의 단방향 흐름과 다름. gateway 내부 `attemptCtx` 변수 관리 필요 (`OnAttemptEnd` 는 ctx 반환 없음).
- `pkg/tracing/oteltracing` 의 OTel API 버전이 caller 의 OTel SDK 버전과 충돌 가능 (`go.sum` diamond dependency). ADR-002 의 "minimal external" 우려가 sub-package 로 격리돼 코어엔 영향 없으나, `oteltracing` 사용자는 버전 pin 이 필요할 수 있음.

### Risks

| Risk | Mitigation |
|---|---|
| OTel global provider 미설정 시 span 생성됐다 즉시 discard — 조용한 no-op | `OtelTracingHook.New()` godoc 에 명시. 사용자가 `otel.SetTracerProvider` 호출 전에는 span 이 기록되지 않음. `examples/tracing/` 에 setup 패턴 포함. |
| `OnAttemptStart` 가 반환하는 `attemptCtx` 가 root ctx 에 span 두 개 (root + attempt) 를 중첩 — `trace.SpanFromContext` 가 항상 deepest span 반환하므로 의도대로 동작. 그러나 `OnAttemptEnd` 에서 `attemptCtx` 가 아닌 `ctx` 를 잘못 넘기면 root span 이 attempt span 처럼 end 됨 | gateway 구현 시 `attemptCtx` 와 `ctx` 혼동 방지를 위해 attempt loop 를 별 함수로 추출 (`runAttempt(ctx, ...) context.Context`) — ctx scoping 이 함수 경계로 명확해짐. |
| `oteltracing` sub-package 의 OTel API minor version 이 caller 의 SDK 와 misalign | `go.mod` 에 `go.opentelemetry.io/otel` version 을 latest stable 로 pin. OTel API 는 semver-stable 이므로 minor bump 에서 API 파괴 없음. major bump (v2) 는 별 ADR. |
| streaming 에서 `OnChatEnd` 가 producer goroutine 종료 시점에 호출 — root span 수명이 `ChatStream` 호출 반환 시점이 아닌 stream 전체 수명과 같음. span 이 수십 초 open 상태 가능 | 의도된 동작. span 은 "logical operation" 의 수명을 따라야 함. streaming 의 논리적 끝은 stream close. 단 trace backend 가 open span timeout 설정이 있으면 조기 종료 가능 — `oteltracing` godoc 에 "streaming span may last O(seconds)-O(minutes)" 명시. |
| Exemplar bridge 에서 traceID 추출 시점에 span 이 이미 ended — `ExtractTraceID` 가 `OnAttemptEnd` 이후 호출되므로 span.End() 직후임. OTel SDK 에서 ended span 의 `SpanContext()` 는 여전히 유효 (`TraceID`, `SpanID` 는 immutable) — 이 호출은 안전. | OTel spec 에서 `SpanContext` 는 span 의 lifecycle 과 무관하게 read-only 접근 보장. 구현 시 `sc.IsValid()` check (`ExtractTraceID` 에 이미 포함) 로 noop span 방어. |

---

## Alternatives Considered

### Alt 1 — Q2 API-only 를 코어 `go.mod` 에 직접 포함

OTel API 는 ~300KB 로 가볍다. 코어에 넣으면 사용자가 별 import 없이도 기본 span 이 동작.

**Reject 이유:** "가볍다" 의 기준이 context-dependent. OTel 없는 Go 서비스 (CLI, 임베디드, 레거시 monolith) 에서 gateway 를 쓸 때 300KB indirect dep 가 눈에 띈다. sub-package 패턴이 선택성 보장. `NoOpTracingHook` 이 default 이므로 OTel-free 사용자에게 sub-package 는 invisible.

### Alt 2 — Q3 Provider 내부 HTTP span (otelhttp transport)

`otelhttp.NewTransport` 를 provider 의 `http.Client` 에 wrapping 해 vendor HTTP call 에 span 추가.

**Reject 이유:** (1) provider 가 현재 `http.Client` 를 외부에서 주입받지 않음 — 내부 구현 변경 필요 = ADR-002 provider interface 와 무관한 내부 변경. (2) LLM vendor 는 traceparent 를 전파하지 않으므로 span 이 vendor 내부를 따라가지 않음 — span 이 있어도 "vendor HTTP call start/end" 의 duration 정보만 얻음. 그건 Q3 의 attempt child span 에서 이미 측정하는 것과 동일한 granularity.

### Alt 3 — Q6 OTel metrics 를 `MetricRecorder` 의 OTel 구현으로 통합

`oteltracing.MetricRecorder` 가 `MetricRecorder` 인터페이스를 구현하고 OTel metrics API 로 publish.

**Reject 이유:** 별도 Q 가 필요한 ADR 범위 확장. ADR-006 Q1 이 이미 "Recorder 인터페이스 + OTel adapter 별 모듈" 을 future-proof 로 남겼다. 그 결정은 유효하고 이 ADR 의 `TracingHook` 분리 패턴으로도 동일한 extensibility 가 보장된다. OTel metrics bridge 는 후속 ADR (v0.3 후보) 에서.

### Alt 5 — Q6 ExemplarTarget push-callback (채택 후 폐기)

최초 설계에서 `oteltracing.ExemplarTarget` 인터페이스를 정의하고, `OtelTracingHook.OnAttemptEnd` 가 `ExemplarTarget.RecordExemplar(traceID)` 를 호출 → `PromRecorder` 가 내부 상태에 traceID 저장 → 다음 `OnAttempt` 호출 시 첨부 하는 push-callback 패턴을 검토했다.

**Reject 이유:** concurrent request 환경에서 Request A 의 `RecordExemplar` 가 Request B 의 `OnAttempt` 사이에 도착하면 traceID 가 교차 오염된다. goroutine ID 는 Go 공개 API 에 없고, ctx 는 immutable 이라 RecordExemplar 호출 시 ctx 에 저장 불가 — per-request 격리 구현이 근본적으로 불가능하다. `TraceIDExtractor` + gateway orchestration 이 stateful bridge 자체를 제거해 race 문제를 구조로 해소한다.

### Alt 4 — Q5 ctx-preserving AsyncWrapper

channel 에 `(ctx, event)` 쌍을 enqueue 하되 ctx 를 WithoutCancel 로 복사해 staleness 방지.

**Reject 이유:** `context.WithoutCancel` (Go 1.21+) 이 cancellation 은 막지만 span 은 ctx 에 value 로 저장된 pointer — span 이 ended 됐을 가능성은 여전히 남는다. span 의 lifecycle 은 ctx cancellation 과 독립적으로 관리되므로 WithoutCancel 이 span staleness 를 해결하지 못함. Q5 의 `TracingHook` 분리 (동기 호출) 이 근본 해결.

---

## Related ADRs

- [ADR-002](0002-provider-interface-design.md) — "외부 의존성 최소화" 원칙. Q2 의 코어 zero 의존 결정의 근거.
- [ADR-005](0005-distributed-rate-limit.md) — `RateLimiter` 인터페이스 + `RedisBackend` 별 구현 패턴. Q2 의 `TracingHook` + `OtelTracingHook` 분리 패턴의 선례.
- [ADR-006](0006-observability.md) — `MetricRecorder` 인터페이스 + Prometheus 기본 구현. Q2 / Q5 / Q6 의 직접 선행 ADR. Q6 의 `ExemplarRecorder` 가 `StreamingMetricRecorder` 패턴 (ADR-006 ↔ ADR-007) 을 재사용.
- [ADR-007](0007-streaming.md) — `ChatStream` 의 pre-stream failover 원칙. Q3 의 attempt child span 범위를 pre-stream phase 에 한정하는 결정의 근거.

## Open Questions

- [ ] **OTel LLM semantic conventions** (`gen_ai.*`) 이 stable 확정 시 `llm.*` attribute prefix rename — 별 ADR 또는 minor PR. 현재 OTel semconv 는 draft 상태 (2026-06 기준).
- [ ] **`oteltracing` 의 별 Go module 분리** — v0.3 에서 release cadence 가 코어와 분리될 필요가 생기면 별 module (`go-llm-gateway/oteltracing v0.x`). 현재 sub-package 로 충분.
- [ ] **span Start/End benchmark** — 실 SDK `TracerProvider` (OTLP exporter) 환경에서 `gateway.Chat` p99 tail latency 오버헤드 측정 필요. `go test -bench=BenchmarkGatewayChat -benchtime=5s` + `NoopProvider` vs `SDKProvider` 비교. 구현 PR 에서 측정 후 README 에 수치 인용. 현재 Negative 항목의 "1–10 µs" 추정치는 실측 전까지 잠정치.
- [ ] **Metrics bridge ADR** — `oteltracing.MetricRecorder` (OTel metrics API 구현) 설계. 본 ADR 의 후속.
- [ ] **Logs bridge ADR** — ADR-006 Q6 Alt 8 의 OTel Logs adapter. 본 ADR 의 후속.
- [ ] **streaming span 수명 경고 임계치** — root span 이 수십 분 open 이면 trace backend 가 GC 로 purge. 구현 시 span events 의 timestamp 으로 대체 가능한지 검토.
