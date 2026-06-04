# ADR-006 — Observability Strategy

- **Status:** Proposed *(agent-generated draft, pending maintainer review)*
- **Date:** 2026-06-04
- **Author:** Junyoung
- **비고:** 본 draft 의 결정/근거는 에이전트가 사용자 요청으로 작성. 각 Q 의 `Maintainer note` 줄을 본인 voice 로 채우기 전엔 `Status` 를 `Accepted` 로 바꾸지 말 것.

---

## Context

Gateway 가 multi-vendor failover + per-key rate limit 까지 갖춘 v0.1.0-rc 상태다 (ADR-001~005 완료). 운영자가 "어느 vendor 가 언제 어떤 비율로 실패하나", "사전 차단 vs 실제 vendor 429 비율" 같은 질문을 답할 수단이 아직 없다. 본 ADR 은 그 측정/노출 메커니즘을 결정.

영향 모듈:

- `pkg/metrics` (신규) — metric recorder 인터페이스 + Prometheus 기본 구현 + `AsyncWrapper` (느린 backend 격리)
- `pkg/types` (신규) — `AttemptInfo` / `FailoverInfo` / `Outcome` / `Origin` 공유 타입. provider / metrics / gateway 모두 단방향 import
- `pkg/provider` — `ChatResponse.Attempts []provider.AttemptInfo` 필드 추가 (단방향 import: provider → types)
- `gateway.Chat` — attempt 단위 측정 hook 추가 (rate limit / vendor 응답 / failover)
- structured log 필드 표준 — **신규 패키지 없음**, Q5 에서 slog 필드 명세만 land (logging 패키지 신설은 over-engineering, slog 가 stdlib)

추가 제약:

- ADR-004 Q5 의 미룬 결정 (attempt trace 노출 형식) 본 ADR 에서 land
- ADR-005 Q7 의 metric label 의도 (pre-empt 차단 vs vendor 429 origin 구분) 본 ADR 에서 실체화
- ADR-001 의 "production-grade" 정체성 — Prometheus + structured log 가 사실상의 산업 표준 default
- 외부 의존성 최소화 (ADR-002 의 stdlib + minimal external 원칙)

---

## Decision

### Q1. Metric 라이브러리 선택

- **Decision:** B — **표준 interface 정의 + Prometheus client_go 기본 구현 동시 제공**.
- **Agent reasoning:** 세 가지 후보 비교:
  - **Prometheus client_go only** — Go 진영에서 사실상 표준. 단 호출자가 OpenTelemetry / Datadog SDK 를 쓰면 어댑터 작성 부담.
  - **OpenTelemetry only** — vendor-neutral, future-proof. SDK + exporters 는 무겁지만 (~5MB), **API-only 모듈 (`go.opentelemetry.io/otel/metric`) 만 의존하면 수백 KB 수준** — caller 가 SDK / exporter 를 inject 하는 bridge 패턴 가능. 그러나 (1) OTel Metric API 는 본 라이브러리의 `Outcome` / `Origin` 같은 domain-specific 의미를 표현하기에 generic 함 (counter / histogram 만 노출, observability event 의 semantic context 는 caller 가 매핑 부담), (2) OTel API 를 직접 base 로 두면 caller 의 backend 선택 자유가 줄어듬 (예: Prometheus 만 쓰는 사용자가 OTel SDK 도입 부담). Recorder 인터페이스 + OTel adapter 별 모듈 패턴이 둘 다의 자유를 살림.
  - **인터페이스 + Prometheus 기본** ✅ — `MetricRecorder` 인터페이스가 base. 호출자는 직접 구현하거나 (예: OTel adapter) 라이브러리가 제공하는 Prometheus 구현 사용. Rate Limit 의 backend pluggable 패턴 (ADR-005 Q1) 과 일관.
- **왜 이 결정이 정당한가:** Prometheus 가 default 인 게 진입성 측면에서 압도적이지만, 한 vendor 에 hard-bind 되면 OpenTelemetry / Datadog 사용자의 진입 장벽. 인터페이스 base + 기본 구현 동시 제공이 ADR-005 의 패턴과 일관.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q2. Metric 셋 (어떤 metric 을 노출하나)

- **Decision:** **Counter 3 + Histogram 1** — 최소 셋:

  | metric | type | labels | 의미 |
  |---|---|---|---|
  | `llm_gateway_requests_total` | counter | `vendor`, `model`, `outcome`, `origin` | 모든 attempt 의 결과 (vendor 별 분해) |
  | `llm_gateway_failovers_total` | counter | `from_vendor`, `to_vendor`, `reason` | failover trigger 발생 횟수 (cascading 분석) |
  | `llm_gateway_unknown_model_total` | counter | `vendor` | 어댑터가 모르는 model id 호출 횟수 (Q7 의 whitelist silencing 보완 — unknown bucket 비정상 증가 alert) |
  | `llm_gateway_attempt_duration_seconds` | histogram | `vendor`, `model`, `outcome` | per-attempt 응답시간 (p50/p95/p99) |

- **Agent reasoning:** 운영 dashboard 의 정형화된 4가지 질문에 답:
  - "현재 어느 vendor 가 실패하나?" → `requests_total{outcome="error"}` by vendor
  - "failover 가 얼마나 발동하나?" → `failovers_total`
  - "사전 차단이 vendor 429 보다 얼마나 자주 발생하나?" → `requests_total{origin="gateway-preempt"}` vs `requests_total{origin="vendor", outcome="error_rate_limit"}`
  - "어느 vendor 가 느린가?" → `attempt_duration_seconds` p99 비교
  - Histogram 만 1개로 줄임 — token usage histogram 은 비용 분석에 유용하지만 본 ADR 에서는 핵심 운영 metric 만 우선 (Q4 의 비용 attribution 은 별도 ADR 후속).
- **왜 이 결정이 정당한가:** metric 수가 많으면 cardinality 폭발 (Q7) + 운영 부담. 정형화된 4가지 질문에 답하는 최소 셋이 production gateway 의 베이스라인.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q3. Label `origin` — 사전 차단 vs vendor 429 분리

- **Decision:** **`origin` label 3-state**: `vendor`, `gateway-preempt`, `gateway-router`.
  - `vendor` — provider 가 응답 (성공이든 vendor-side 에러든)
  - `gateway-preempt` — rate limit pre-check 가 차단 (ADR-005 Q7 의 사전 차단)
  - `gateway-router` — 라우팅 실패 (no provider supports model, ADR-003 Q3)
- **Agent reasoning:** ADR-005 Q7 결정 ("사전 차단도 vendor 429 와 같은 sentinel `ErrRateLimited`") 의 trade-off — call site 코드 통일성을 위해 sentinel 통합. 그 trade-off 의 보완책으로 metric 에서 origin 분리.
  - 사전 차단이 많아지면 → vendor quota 가 trafic 보다 작거나 / rate limit config 가 너무 보수적
  - vendor 429 가 많아지면 → rate limit 이 underestimating (예: char/4 휴리스틱이 실제 토큰보다 작음, ADR-005 Q3) → tokenizer 교체 우선순위 상승
  - 두 케이스가 metric 에서 안 분리되면 운영자가 디버깅 시 가설 좁힐 수단 없음.
- **왜 이 결정이 정당한가:** ADR-005 의 sentinel 통합이 call site 단순화에 옳지만, 그 정보 손실을 metric label 로 보완. 세 origin 의 비율이 운영 가설의 직접 입력.
- **Sub-decision (ADR-005 fail-open path):** rate limit backend 가 에러 → FailOpen default → vendor 호출 진행. 이 경로의 origin 은 `vendor` (vendor 가 응답하므로). backend 에러 자체는 **`slog.Warn` 으로 surface (metric 신설 X)** — backend 에러는 운영자 alert 의 1차 채널이 log (Loki / Grafana log panel) 이고, metric 신설은 cardinality 추가 + dashboard 복잡도만 늘림. 4번째 origin state 도입은 사용자 mental model 만 복잡하게 함.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q4. Attempt trace 노출 방식 (ADR-004 Q5 의 미룬 결정)

- **Decision:** B — **`ChatResponse.Attempts []AttemptInfo` 필드 + recorder hook 동시**.
  - `ChatResponse.Attempts` — caller 가 응답에서 직접 읽음 (예: debugging log)
  - `MetricRecorder.OnAttempt(info AttemptInfo)` — metric / log integration 의 single source of truth
- **Agent reasoning:** 세 가지 옵션 비교:
  - **A. Response 필드만** — caller 가 직접 처리. 메트릭 집계는 caller 책임. 메트릭 라이브러리 의존성 없음. 단점: 매 caller 마다 metric 코드 중복.
  - **B. Recorder hook 만** — gateway 내부 metric recorder 가 attempt 마다 콜백. caller 는 모름. 단점: caller 가 trace 정보 필요할 때 (response logging, request_id correlation) 별도 channel 필요.
  - **C. 둘 다** ✅ — Response 필드는 caller-facing, Recorder hook 은 metric/log facing. 의도가 명확.
- **왜 이 결정이 정당한가:** ADR-004 가 미룬 이유 (metric 모듈의 요구 사항이 입력) 가 이제 명확해짐 — recorder 패턴이 metric/log 의 공통 sink, response 필드가 caller 의 debugging 용. 두 path 가 서로 직교 (사용 케이스 다름):
  - **Concrete operator story for "Response 필드"**: HTTP handler 가 caller (예: chat API) 의 response body 에 `attempts: [...]` 를 그대로 노출 — 사용자 UI 가 "이 응답이 어느 vendor 에서, 몇 번 시도 후 왔는지" 를 즉시 표시. Log pipeline 의 ingestion lag (보통 수 초~분) 없이 same-request scope 의 정보 노출. Recorder hook 만 있으면 이 use case 가 log → polling 으로 우회되어야 함.
  - **Concrete operator story for "Recorder hook"**: 운영자가 Grafana 에서 vendor 별 실패율 alert 보고 디버깅 — request_id 별 individual response 가 아니라 집계된 metric 이 1차 sink. Response 필드만 있으면 매 caller 가 metric 집계 코드 작성 (DRY 위반).
  - 두 use case 의 audience 가 다르므로 (사용자 UI vs 운영자 dashboard) "직교" — diverge 위험은 caller mutation 의 trade-off 로 인정 (sub-decision 참고).
- **Sub-decision (타입 위치, 분할):** trace primitive 들을 SRP 기준으로 **두 패키지에 분할**:

  | 타입 | 위치 | 근거 |
  |---|---|---|
  | `Outcome`, `Origin` | `pkg/types` (신규, leaf) | 순수 metric label semantic. vendor wire 와 무관. `OriginGatewayPreempt` 같은 gateway-internal 개념이 provider 에 들어가는 건 SRP 위반 — 별 leaf 패키지에 격리. |
  | `AttemptInfo`, `FailoverInfo` | `pkg/provider` | `provider.Usage` / `*provider.ProviderError` / `provider.ErrorType` 을 composition 으로 보유. 같은 패키지에 두는 게 자연. `types.Outcome` / `types.Origin` 만 import (leaf 의존). |

  의존성 그래프 (최종):
  ```
  pkg/types  (leaf, Outcome/Origin)
      ↑
  pkg/provider  (AttemptInfo, FailoverInfo, ChatResponse.Attempts)
      ↑
  pkg/metrics  (MetricRecorder, types/provider 양쪽 import)
      ↑
  pkg/gateway  (Chat, 모두 import)
  ```
  - 순환 없음. 각 레이어가 자기 책임만 소유 (types=label enum, provider=wire contract + composition, metrics=recorder, gateway=orchestration).
  - 대안 옵션 (`pkg/metrics` / `pkg/gateway` / `pkg/provider` 전체) 모두 SRP 또는 순환 위반 — 분할 선택이 valid.
  - 추가 비용: 신규 `pkg/types` 1개. 매우 얇음 (2 enum). caller import 한 줄 추가.

- **Sub-decision (Attempts 슬라이스 mutation):** `ChatResponse.Attempts` 는 caller-owned 슬라이스. caller 가 응답 받은 후 element 를 mutate 해도 라이브러리는 영향 없음 (다음 호출에 재사용 안 함). 단 **recorder 가 이미 emit 한 metric 과 응답 필드가 diverge** 할 수 있음 — 이건 Q4 의 trade-off 로 인정. caller 가 응답을 다른 곳에 forward 할 때 mutation 안 하는 게 정상 사용. mutation 검출 / freeze 는 over-engineering (Go 에 immutable slice 표준 없음).
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q5. Structured logging 표준

- **Decision:** **`log/slog` 표준 필드 명세 + 분리된 `LogRecorder` 모듈**.
  - **타입명 (구현 결정):** `LogRecorder` — 초기 draft 의 `LoggingRecorder` 는 동명사 접두(`Logging`)가 동작/주체 둘 다로 해석 가능해 모호. PromRecorder 와 cadence 맞춘 명사형이 역할 (record-keeper) 을 더 직접적으로 표현. ADR 본문은 구현 landing 시점에 일괄 rename — 추후 grep / search 의 zero-result 방지.
  - 표준 필드: `request_id`, `vendor`, `model`, `attempt`, `outcome`, `duration_ms`, `origin`
  - `LogRecorder` (별 모듈, `pkg/metrics/logrecorder`) 는 `MetricRecorder` 인터페이스 구현체로, OnAttempt 마다 위 필드로 `slog.Info` / `slog.Warn` emit
  - **MetricRecorder 자체는 slog 책임 없음** — 사용자가 metric 만 / log 만 / 둘 다 자유롭게 조합. MultiRecorder 로 합성 가능 (`metrics.Multi(promRecorder, logrecorder.New())`)
- **Agent reasoning:** 초기 draft 는 OnAttempt 가 slog 도 emit 하는 책임 결합. 그러면 `Config.Metrics = nil` (NoOpRecorder) 사용자는 metric 도 log 도 둘 다 잃음 — log-only 사용 케이스 불가. 책임 분리 후:
  - Metric 만: `prom.New()`
  - Log 만: `logrecorder.New()`
  - 둘 다: `metrics.Multi(prom.New(), logrecorder.New())`
  - 둘 다 없음: nil → NoOp
- **왜 이 결정이 정당한가:** SRP (single responsibility) — recorder 가 metric backend 추상화이고, log emission 은 별 책임. 분리하면 caller 의 조합 자유 + nil recorder 가 명시적 의미 (둘 다 비활성) 유지.
- **Sub-decision (인터페이스 conflation 의 인정된 trade-off):** semantic 으론 metric (집계 숫자) ≠ log (개별 이벤트) — 관찰 주기 / 보존 정책 / drop 허용도 다름. 그럼에도 두 구현체를 동일 `MetricRecorder` 인터페이스로 통합하는 이유:
  - **조합 패턴 재사용** — `metrics.Multi(prom, log)` 가 fan-out 의 single source of truth. 별 `LogRecorder` 인터페이스 + 별 `LogMultiRecorder` 도입은 같은 패턴 두 번 작성 + caller 가 두 필드 (`Config.Metrics` + `Config.Logger`) 채우는 부담.
  - **인터페이스 시그니처 동일** — 둘 다 `OnAttempt(ctx, AttemptInfo)` / `OnFailover(ctx, FailoverInfo)` 로 충분. 추상화가 자연.
  - **인정된 비용** — `Config.Metrics` 에 `LogRecorder` 가 들어가면 독자가 "metric backend" 로 오독 가능. doc + 변수명 (`MetricRecorder` → 향후 `ObservabilityRecorder` rename 후보) 으로 완화. 본 ADR 의 trade-off 인정은 향후 인터페이스 분리 PR 이 별 ADR 없이 시도되지 않도록 trail 보존.
  - **검토된 대안**: `slog.Handler` 미들웨어 패턴으로 LogRecorder 자체를 우회 → Alt 7. 별도의 `LogRecorder` 인터페이스 (MetricRecorder 와 분리된 독립 추상화) → 본 ADR 에서 미채용 (`Config.Metrics` + `Config.Logger` 두 필드 부담 + `Multi` 패턴 중복 작성 비용 > 인터페이스 conflation 의 오독 비용).
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q6. Distributed tracing (OpenTelemetry spans)

- **Decision:** **v0.1 에 포함 X** — Recorder 인터페이스가 향후 OTel adapter 의 확장점.
- **Agent reasoning:** OTel span 도입은 매력적이지만:
  - 외부 의존성 무겁다 (otel-go SDK + propagators 합치면 +5MB)
  - context propagation 룰이 OTel spec 종속 (예: span 이 ctx 에 어떻게 carried 되는지)
  - 사용자가 OTel 안 쓰면 dead weight
  - 본 Recorder 인터페이스가 OnAttempt 콜백을 제공하므로, OTel adapter 가 별 모듈 (`pkg/metrics/otel`) 로 적층 가능 — 라이브러리 코어에 강제 의존성 X.
- **왜 이 결정이 정당한가:** YAGNI. OTel 수요가 실제 발생할 때 별 ADR 로 도입 (v0.2 후보). 본 ADR 은 Recorder 추상화로 future-proof.
- **Sub-decision (AsyncWrapper ↔ OTel ctx staleness):** AsyncWrapper 를 통한 ctx 는 consumer 가 꺼낼 때 cancelled 가능 — OTel 의 `trace.SpanFromContext(ctx)` 가 parent span 없는 상태. 두 가지 해결책 검토:
  - **A. `AttemptInfo` 에 trace snapshot 필드** (`TraceID` / `SpanID` string) — gateway 가 OnAttempt 호출 직전 스냅샷. reject 사유: Q6 본 결정이 "v0.1 에 OTel 미포함" 인데 v0.1 의 AttemptInfo 에 OTel vocabulary 를 land 하는 건 YAGNI 자기모순. 실제 span 추출 로직 없으면 두 필드는 항상 빈 문자열 → dead weight. ADR-009 (OTel) 시점에 land 가 맞음.
  - **B. AsyncWrapper + OTel 조합 사용 금지** ✅ — `AsyncWrapper` godoc 에 명시: "OTel / 기타 trace-context-coupled backend 와 함께 사용 금지 — ctx staleness 로 parent span 유실". OTel adapter 사용자는 sync 호출 (느린 backend 인 경우 자체 buffer 구현) 또는 ADR-009 에서 별 wrapper 도입.
  - **선택: B.** Q6 의 YAGNI 결정과 일관. ADR-009 에서 OTel 도입 시 인터페이스에 trace snapshot 필드 추가 또는 별 ctx-preserving wrapper 도입 — 그 시점에 land.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q7. Cardinality 폭발 방지

- **Decision:** **`key_hash` 는 metric label 에서 제외, `model` 은 표준 모델명만 허용**.
  - `vendor` label (또는 `from_vendor` / `to_vendor` for failovers_total) — provider.Name() (예: "openai", "anthropic") — 카디널리티 ≤ 10
  - `reason` label (failovers_total) — ErrorType 9개 — 카디널리티 ≤ 10
  - `model` label — vendor 의 표준 model id ("gpt-4o", "claude-opus-4-7" 등) — 카디널리티 ≤ 50
    - **whitelist 유래**: `Provider.SupportsModel(model)` 이 true 인 model id 만 그대로 통과, 나머지는 `unknown` 으로 fallback. SupportsModel 의 실제 데이터 소스는 어댑터 구현에 따라 다름:
      - 정적 (build-time) — `gpt-4o`, `gpt-4o-mini` 같은 안정 family 는 어댑터에 하드코딩
      - 반-동적 — 어댑터 옵션 (`WithModels([]string)`) 으로 caller 가 등록. date-versioned ID (`gpt-4o-2024-11-20`) 는 caller 가 명시 등록 → redeploy 없이 추가 가능
    - **운영 가시성 보완**: `llm_gateway_unknown_model_total{vendor}` counter 로 unknown bucket 증가 추적. 비정상 증가 시 alert → 어댑터 옵션 추가하거나 어댑터 패치. ADR-001 의 "production-grade" 정체성과 충돌 방지.
    - **발화 책임 (gateway 레이어):** model normalize 는 **gateway 가 담당** — `Config.KnownModels []string` 옵션 (default empty) 으로 caller 가 화이트리스트 등록. `gateway.Chat` 이 AttemptInfo 를 만들 때 `info.Model` 이 known set 에 없으면 `"unknown"` 으로 normalize 후 recorder hook 호출. 결과: 모든 recorder (PromRecorder / LogRecorder / Multi) 가 **동일한 normalized model** 수신 — Q5 의 metric/log asymmetry 자동 해결.
    - **`unknown_model_total` 발화:** PromRecorder.OnAttempt 에서 `info.Model == "unknown"` 일 때 `unknownModelTotal{vendor}++`. gateway 가 normalize 했으므로 PromRecorder 는 단순 check.
    - **Default whitelist:** `Config.KnownModels` 의 기본값은 **빈 set** — 등록 안 하면 모든 호출이 `"unknown"` 매핑 (cardinality 보호 + 운영자에게 alert 즉시). 어댑터에 hardcoded fallback (`gateway.WithDefaultKnownModelsFromAdapters`) 옵션은 v0.2 후보.
    - **LogRecorder 의 asymmetric cost:** normalize 가 gateway 레이어이므로 LogRecorder 도 `model="unknown"` 만 받음 — cardinality 가 비용 요인이 아닌 log 측 사용자도 KnownModels 미등록 시 log 의 `model` 필드가 전부 `"unknown"` 으로 찍힘 (Loki / CloudWatch Logs Insights 에서 model 별 필터 / facet 불가). log 포렌식이 필요한 caller 는 KnownModels 등록 필수. 이 asymmetry 가 의도된 비용 — 인터페이스 conflation (Q5 sub-decision) 의 follow-on. 대안: LogRecorder 가 raw model 받는 별 normalize 우회는 "모든 recorder 가 동일 input" 원칙 위반 + 인터페이스 시그니처에 raw/normalized 둘 다 싣는 부담.
    - **이전 round 의 PromRecorder-내 whitelist 결정 reposition:** 초기 draft 는 PromRecorder.WithKnownModels 였음. critic 의 지적 (PromRecorder / LogRecorder asymmetry: log 측은 raw model 받음, metric 측은 normalize) 수용해 책임을 gateway 로 상향. recorder 인터페이스 변경 없음 (input data 만 일관).
    - **date-versioned model 의 운영 비용**: OpenAI 가 매월 새 model snapshot 출시 → 안 등록하면 silent unknown. 권장 운영 패턴: alert threshold = unknown_total 이 5분간 ≥ 100 → on-call 이 어댑터 옵션 추가하거나 model alias 확인.
  - `outcome` label — `success` 1개 + ErrorType 9개의 `error_<type>` (아래 enumeration) — 카디널리티 ≤ 10
    | outcome value | trigger |
    |---|---|
    | `success` | provider.Chat 가 nil 에러 반환 |
    | `error_rate_limit` | ErrorTypeRateLimit (vendor 429 또는 gateway pre-empt) |
    | `error_auth` | ErrorTypeAuth (401) |
    | `error_overloaded` | ErrorTypeOverloaded (Anthropic 등) |
    | `error_server` | ErrorTypeServer (5xx) |
    | `error_timeout` | ErrorTypeTimeout (ctx deadline / transport) |
    | `error_invalid_input` | ErrorTypeInvalidInput (400 / 라우팅 실패) |
    | `error_not_found` | ErrorTypeNotFound (404) |
    | `error_permission` | ErrorTypePermission (403) |
    | `error_unknown` | ErrorTypeUnknown 또는 *ProviderError 아닌 raw error |
  - `origin` label — 3-state — 카디널리티 = 3
  - `key_hash` 는 라벨 X — 비용 attribution 이 필요하면 별 metric (예: `llm_gateway_cost_usd_by_key_total`) 로 분리, 본 ADR 에서는 미포함.
- **Agent reasoning:** Prometheus 의 cardinality 한도 (보통 ≤ 100k unique label combinations per metric) 를 안전하게 유지. `key_hash` 를 label 로 넣으면 사용자 수만큼 카디널리티 폭발 → Prometheus storage cost / Grafana query latency 증가. 비용 attribution 은 별 use case 라 별 metric 으로 분리.
- **왜 이 결정이 정당한가:** Prometheus best practice ("don't put high-cardinality data in labels"). per-key 비용 attribution 이 critical 한 사용자는 별 metric 으로 opt-in. 기본 셋은 운영 dashboard 의 baseline 만 cover.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

---

### Synthesis — Go pseudocode

```go
// 의존성 방향 (순환 방지, 최종 layout):
//   - pkg/types — leaf. Outcome / Origin enum 만. import 없음.
//   - pkg/provider — types 를 import (단방향). AttemptInfo / FailoverInfo 정의 + Usage /
//     ProviderError / ErrorType / ChatResponse.Attempts.
//   - pkg/metrics — types + provider 둘 다 import (단방향).
//   - pkg/gateway — 모두 import (단방향).
//
// Outcome / Origin 이 pkg/types 인 이유: pure metric label semantic (vendor wire 와 무관).
// AttemptInfo 가 pkg/provider 인 이유: Usage / ProviderError 같은 provider 타입을 composition
// 으로 보유 — 같은 패키지에 두는 게 자연스러움. 순환 없이 SRP 분리.

// --- pkg/types ---

package types

// Outcome / Origin 은 metric 분류 enum. vendor wire 와 무관, gateway runtime 의 분류
// 의미만. leaf package — import 없음.

type Outcome string

const (
	OutcomeSuccess           Outcome = "success"
	OutcomeErrorRateLimit    Outcome = "error_rate_limit"
	OutcomeErrorAuth         Outcome = "error_auth"
	OutcomeErrorOverloaded   Outcome = "error_overloaded"
	OutcomeErrorServer       Outcome = "error_server"
	OutcomeErrorTimeout      Outcome = "error_timeout"
	OutcomeErrorInvalidInput Outcome = "error_invalid_input"
	OutcomeErrorNotFound     Outcome = "error_not_found"
	OutcomeErrorPermission   Outcome = "error_permission"
	OutcomeErrorUnknown      Outcome = "error_unknown"
)

type Origin string

const (
	OriginVendor         Origin = "vendor"          // provider 가 응답 (성공 / vendor-side 에러)
	OriginGatewayPreempt Origin = "gateway-preempt" // rate limit pre-check 차단
	OriginGatewayRouter  Origin = "gateway-router"  // router 라우팅 실패
)

// --- pkg/provider ---

package provider

import (
	"errors"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/types"
)

// AttemptInfo 는 한 vendor 시도의 결과. ChatResponse.Attempts 에 들어가는 entry.
// Usage / *ProviderError 같은 provider-domain 타입을 composition 으로 보유하므로
// 같은 패키지에 두는 게 자연. Outcome / Origin 만 pkg/types 에서 import.
type AttemptInfo struct {
	Vendor   string         // Provider.Name()
	Model    string         // ChatRequest.Model
	AttemptN int            // 0=primary, 1+=fallback
	Outcome  types.Outcome  // success / error_<type>
	Origin   types.Origin   // vendor / gateway-preempt / gateway-router
	Duration time.Duration
	Usage    Usage          // 성공 시
	Error    *ProviderError // 실패 시
}

type FailoverInfo struct {
	FromVendor string
	ToVendor   string
	Reason     ErrorType
}

// OutcomeFromErr 는 *ProviderError 의 Type 을 types.Outcome 으로 매핑. provider
// 가 errors.As 로 *ProviderError 추출 후 pe.Type 으로 switch — 매칭 안 되면
// types.OutcomeErrorUnknown. Outcome / Origin 자체는 pkg/types 에 정의 (위 sub-
// decision 참고), 이 헬퍼는 mapping 만 담당.
func OutcomeFromErr(err error) types.Outcome {
	if err == nil {
		return types.OutcomeSuccess
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		return types.OutcomeErrorUnknown
	}
	switch pe.Type {
	case ErrorTypeRateLimit:
		return types.OutcomeErrorRateLimit
	case ErrorTypeAuth:
		return types.OutcomeErrorAuth
	case ErrorTypeOverloaded:
		return types.OutcomeErrorOverloaded
	// ... (9개 ErrorType 매핑)
	default:
		return types.OutcomeErrorUnknown
	}
}

// --- pkg/metrics ---

package metrics

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/types"
)

// MetricRecorder 는 gateway 가 매 attempt / failover 마다 호출하는 콜백.
//
// CONTRACT: 구현체는 *반드시* 즉시 반환해야 한다. blocking work (remote exporter
// flush, slow IO, sync.Mutex 경쟁) 가 필요하면 내부 buffer / goroutine 으로
// 격리해야 한다. gateway 는 caller goroutine 에서 동기로 호출하므로, 느린
// recorder 는 매 Chat 호출의 latency 에 직접 더해진다. AsyncWrapper (별 모듈)
// 가 buffered channel 기반 표준 격리 패턴을 제공한다.
//
// 빠른 backend 는 동기 호출 OK (Prometheus Counter 는 atomic, NoOp 는 0-cost).
// 느린 backend (OTel remote exporter, Datadog flush) 는 AsyncWrapper 권장.
type MetricRecorder interface {
	OnAttempt(ctx context.Context, info provider.AttemptInfo)
	OnFailover(ctx context.Context, info provider.FailoverInfo)
}

// NoOpRecorder 는 default — Gateway 가 Config.Metrics 가 nil 일 때 사용.
type NoOpRecorder struct{}

func (NoOpRecorder) OnAttempt(context.Context, provider.AttemptInfo)   {}
func (NoOpRecorder) OnFailover(context.Context, provider.FailoverInfo) {}

// 컴파일 타임 contract assertion (NoOpRecorder + MultiRecorder + AsyncWrapper).
var (
	_ MetricRecorder = NoOpRecorder{}
	_ MetricRecorder = MultiRecorder(nil)
	_ MetricRecorder = (*AsyncWrapper)(nil)
)

// MultiRecorder 는 여러 recorder 를 합성 — caller 가 Prometheus + LogRecorder
// 등 둘 다 받을 때 사용.
type MultiRecorder []MetricRecorder

func Multi(recorders ...MetricRecorder) MultiRecorder { return MultiRecorder(recorders) }

// MultiRecorder 의 각 호출은 개별 recover() 로 감싸진다 — A 가 panic 해도 B 는
// 호출됨 (observability 부분 소실 방지).
func (m MultiRecorder) OnAttempt(ctx context.Context, info provider.AttemptInfo) {
	for _, r := range m {
		func(r MetricRecorder) {
			defer func() {
				if p := recover(); p != nil {
					slog.ErrorContext(ctx, "MultiRecorder.OnAttempt: recorder panicked",
						"panic", p, "vendor", info.Vendor)
				}
			}()
			r.OnAttempt(ctx, info)
		}(r)
	}
}
func (m MultiRecorder) OnFailover(ctx context.Context, info provider.FailoverInfo) {
	for _, r := range m {
		func(r MetricRecorder) {
			defer func() {
				if p := recover(); p != nil {
					slog.ErrorContext(ctx, "MultiRecorder.OnFailover: recorder panicked",
						"panic", p, "from", info.FromVendor, "to", info.ToVendor)
				}
			}()
			r.OnFailover(ctx, info)
		}(r)
	}
}

// AsyncWrapper 는 slow recorder (OTel exporter, Datadog flush 등) 를 caller goroutine
// 밖으로 격리. buffered channel + background consumer goroutine. 본 ADR 의 결정으로
// pkg/metrics 안에 land (Open Question 의 backpressure 정책만 향후 별 ADR 후보).
//
// Default backpressure: drop newest (gateway latency 보호 우선). buffer 가 가득 차면
// 새 event 를 drop 하고 dropCounter 를 증가 — 운영자가 dropCounter 로 backpressure
// 발생 모니터링.
//
// 아래는 완전한 struct 정의 (필드는 Close 메서드 / sync.Once 포함):
type AsyncWrapper struct {
	inner       MetricRecorder
	attemptCh   chan asyncAttempt
	failoverCh  chan asyncFailover
	dropCounter atomic.Uint64
	done        chan struct{}
	closeOnce   sync.Once
}

type asyncAttempt struct {
	ctx  context.Context
	info provider.AttemptInfo
}
type asyncFailover struct {
	ctx  context.Context
	info provider.FailoverInfo
}

func NewAsyncWrapper(inner MetricRecorder, bufSize int) *AsyncWrapper {
	w := &AsyncWrapper{
		inner:      inner,
		attemptCh:  make(chan asyncAttempt, bufSize),
		failoverCh: make(chan asyncFailover, bufSize),
		done:       make(chan struct{}),
	}
	go w.consume()
	return w
}

func (w *AsyncWrapper) OnAttempt(ctx context.Context, info provider.AttemptInfo) {
	select {
	case w.attemptCh <- asyncAttempt{ctx, info}:
	default:
		w.dropCounter.Add(1)
	}
}
func (w *AsyncWrapper) OnFailover(ctx context.Context, info provider.FailoverInfo) {
	select {
	case w.failoverCh <- asyncFailover{ctx, info}:
	default:
		w.dropCounter.Add(1)
	}
}

// Close 는 background goroutine 을 종료. caller 의 책임 — Gateway 가 GC 되거나
// 프로세스 종료 전 명시 호출. 이중 호출 안전 (close 된 channel 재close 는 panic
// 이므로 done 채널 sentinel 패턴 사용).
//
// CONTRACT (Close timing):
// Close() 는 *반드시 모든 producer goroutine (= gateway.Chat 호출자) 이 멈춘 후* 호출.
// concurrent producer 가 Close 동시에 OnAttempt 를 호출하면 buffered channel 에 send
// 후 consume goroutine 이 종료된 상태일 수 있어 drain 부분 소실. 본 ADR 은 "graceful
// shutdown" 책임을 caller 에 위임 — gateway runtime 의 중지 sequence:
//   1. 새 gateway.Chat 호출 중단 (앱 레이어)
//   2. 진행 중 호출 완료 대기 (sync.WaitGroup 등)
//   3. AsyncWrapper.Close()
//
// 즉 본 wrapper 는 *모든* event 의 drain 을 보장하지 않으며, race 시 마지막 N event 는
// silent loss 가능 (그러나 production 운영에서는 graceful shutdown 시 이 손실이 무시 가능).
func (w *AsyncWrapper) Close() error {
	w.closeOnce.Do(func() { close(w.done) })
	return nil
}

func (w *AsyncWrapper) consume() {
	for {
		select {
		case <-w.done:
			// drain 남은 event 후 종료.
			for len(w.attemptCh) > 0 || len(w.failoverCh) > 0 {
				select {
				case ev := <-w.attemptCh:
					w.inner.OnAttempt(ev.ctx, ev.info)
				case ev := <-w.failoverCh:
					w.inner.OnFailover(ev.ctx, ev.info)
				}
			}
			return
		case ev := <-w.attemptCh:
			w.inner.OnAttempt(ev.ctx, ev.info)
		case ev := <-w.failoverCh:
			w.inner.OnFailover(ev.ctx, ev.info)
		}
	}
}
func (w *AsyncWrapper) DroppedEvents() uint64 { return w.dropCounter.Load() }
```


**gateway 패키지 (Config / New / ChatResponse 확장):**

```go
package gateway

import (
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/metrics"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/ratelimit"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/types"
)

// Config 확장 — non-breaking.
type Config struct {
	Providers   []provider.Provider
	RateLimit   ratelimit.RateLimiter
	Metrics     metrics.MetricRecorder // 새로 추가, nil 이면 New 가 NoOpRecorder{} 로 대체
	KnownModels []string               // 새로 추가, Q7 — recorder label 에 normalize. 빈 set 이면 모든 호출이 model="unknown"
}

func New(cfg Config) (*Gateway, error) {
	// ... 기존 validation ...
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.NoOpRecorder{}
	}
	known := make(map[string]struct{}, len(cfg.KnownModels))
	for _, m := range cfg.KnownModels {
		known[m] = struct{}{}
	}
	return &Gateway{
		providers: cfg.Providers, rateLimit: cfg.RateLimit, metrics: cfg.Metrics,
		knownModels: known,
	}, nil
}
```

`provider.ChatResponse` 자체에는 `Attempts []provider.AttemptInfo` 필드 추가
— non-breaking. 호출자의 debugging / request_id correlation path.

```go
package provider

type ChatResponse struct {
	Content      string
	FinishReason FinishReason
	Usage        Usage
	Raw          []byte
	Attempts     []AttemptInfo // 새로 추가, 호출자 facing
}
```

**Prometheus 기본 구현 (별 모듈):**

```go
package promrecorder

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/types"
)

func New() *PromRecorder { /* counter / histogram 등록 */ }

// PromRecorder 는 gateway 가 이미 normalize 한 info.Model (unknown 처리 완료) 을
// 받는다 — whitelist 책임은 gateway 레이어 (Q7 sub-decision 참고). PromRecorder 는
// 순수 emit 책임만.
func (r *PromRecorder) OnAttempt(ctx context.Context, info provider.AttemptInfo) {
	r.requestsTotal.WithLabelValues(info.Vendor, info.Model, string(info.Outcome), string(info.Origin)).Inc()
	if info.Model == "unknown" {
		r.unknownModelTotal.WithLabelValues(info.Vendor).Inc()
	}
	// pre-empt / router 경로는 Duration = 0 → histogram 오염 방지 위해 vendor origin
	// 만 observe. attempt_duration_seconds 의 p50/p95/p99 가 0 으로 왜곡되는 걸 회피.
	if info.Origin == types.OriginVendor {
		r.attemptDuration.WithLabelValues(info.Vendor, info.Model, string(info.Outcome)).Observe(info.Duration.Seconds())
	}
}

func (r *PromRecorder) OnFailover(ctx context.Context, info provider.FailoverInfo) {
	// reason label 을 outcome 과 동일 포맷 ("error_<type>") 으로 normalize — dashboard
	// 의 cross-query (failovers_total.reason vs requests_total.outcome) 정합.
	r.failoversTotal.WithLabelValues(info.FromVendor, info.ToVendor, "error_"+string(info.Reason)).Inc()
}
```

**gateway.Chat 통합:**

```go
// normalizeModel 은 Config.KnownModels 화이트리스트 기준으로 model id 를 normalize.
// 등록 안 된 model 은 "unknown" — recorder 가 일관된 값을 받음 (Q7 sub-decision).
func (g *Gateway) normalizeModel(model string) string {
	if _, known := g.knownModels[model]; known {
		return model
	}
	return "unknown"
}

// recordAttempt / recordFailover 는 recorder panic 격리 (Risks 표). 두 hook 모두
// 같은 패턴으로 처리 — 한쪽만 보호하면 OnFailover panic 시 Chat 전체가 crash.
func (g *Gateway) recordAttempt(ctx context.Context, info provider.AttemptInfo) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "metrics.OnAttempt panicked", "panic", r, "vendor", info.Vendor)
		}
	}()
	g.metrics.OnAttempt(ctx, info)
}

func (g *Gateway) recordFailover(ctx context.Context, info provider.FailoverInfo) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "metrics.OnFailover panicked", "panic", r,
				"from", info.FromVendor, "to", info.ToVendor)
		}
	}()
	g.metrics.OnFailover(ctx, info)
}

func (g *Gateway) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	primary, fallbacks, err := router.PickWithFallbacks(g.providers, req.Model)
	if err != nil {
		// origin=gateway-router. Vendor 가 없으므로 빈 문자열, AttemptN=0.
		g.recordAttempt(ctx, provider.AttemptInfo{
			Vendor:  "gateway", // router 실패 — vendor 는 gateway 자체 (router 가 *ProviderError{Vendor:"gateway"} 반환과 일관)
			Model:   g.normalizeModel(req.Model),
			Outcome: provider.OutcomeFromErr(err),
			Origin:  types.OriginGatewayRouter,
			Error:   asProviderError(err),
		})
		return provider.ChatResponse{}, err
	}

	candidates := append([]provider.Provider{primary}, fallbacks...)
	attempts := make([]provider.AttemptInfo, 0, len(candidates)) // 사전 할당
	var lastErr error

	for n, p := range candidates {
		if ctxErr := ctx.Err(); ctxErr != nil { /* dual-wrap ADR-004 */ }

		// rate-limit pre-check (ADR-005). FailOpen 시 backend 에러는 origin=vendor
		// 로 진행 (Q3 의 Open Question 정리 — fail-open 은 vendor 호출로 이어지므로).
		if g.rateLimit != nil {
			if d, _ := g.rateLimit.Allow(ctx, p.Name(), p.KeyHash(), req); !d.Allow {
				info := provider.AttemptInfo{
					Vendor: p.Name(), Model: req.Model, AttemptN: n,
					Outcome: types.OutcomeErrorRateLimit, Origin: types.OriginGatewayPreempt,
				}
				attempts = append(attempts, info)
				g.recordAttempt(ctx, info)
				// ... failover continue
			}
		}

		start := time.Now()
		resp, cerr := p.Chat(ctx, req)
		info := provider.AttemptInfo{
			Vendor: p.Name(), Model: req.Model, AttemptN: n,
			Duration: time.Since(start), Origin: types.OriginVendor,
		}
		if cerr == nil {
			info.Outcome = types.OutcomeSuccess
			info.Usage = resp.Usage
			attempts = append(attempts, info)
			g.recordAttempt(ctx, info)
			resp.Attempts = attempts
			return resp, nil
		}
		info.Outcome = provider.OutcomeFromErr(cerr) // provider 패키지의 exported helper
		info.Error = asProviderError(cerr)            // errors.As(cerr, &pe) wrapping (nil 반환 가능)
		attempts = append(attempts, info)
		g.recordAttempt(ctx, info)

		if !shouldFailover(cerr) { /* abort */ }
		if n+1 < len(candidates) {
			var reason provider.ErrorType
			if info.Error != nil { // asProviderError 가 nil 반환 가능 (비-ProviderError)
				reason = info.Error.Type
			} else {
				reason = provider.ErrorTypeUnknown
			}
			g.recordFailover(ctx, provider.FailoverInfo{
				FromVendor: p.Name(), ToVendor: candidates[n+1].Name(), Reason: reason,
			})
		}
	}
	return provider.ChatResponse{Attempts: attempts}, lastErr
}
```

---

## Alternatives Considered

### Alt 1 — Q1 Prometheus only (no interface)

- **장점:** 단일 의존성, 단순.
- **단점:** OTel / Datadog 사용자가 라이브러리 진입 못 하거나 fork 필요. ADR-005 의 pluggable backend 패턴과 불일치.
- **안 선택한 이유:** Multi-backend 지원이 본 라이브러리 정체성. 인터페이스 base + 기본 구현이 ADR-005 와 일관.

### Alt 2 — Q4 Response 필드만

- **장점:** Recorder 인터페이스 없음 → 라이브러리 코어 단순.
- **단점:** Metric 집계 책임이 caller 에 — 모든 user 가 metric 코드 작성. observability 가 caller-side 의 "옵션" 이 아니라 "공통 책임" 이 됨 → ADR-001 의 "production-grade" 정체성과 충돌.
- **안 선택한 이유:** Recorder 가 metric 의 sink 인 게 library 책임. caller debugging 은 별 path.

### Alt 3 — Q6 v0.1 에 OTel 포함

- **장점:** future-proof + cloud-native 사용자 immediate value.
- **단점:** ~5MB 외부 의존성, ctx propagation 룰이 spec 종속, OTel 안 쓰는 사용자에게 dead weight.
- **안 선택한 이유:** YAGNI. Recorder 인터페이스가 OTel adapter 의 확장점 — 별 모듈로 도입 가능.

### Alt 4 — Q1 비동기 채널 기반 recorder (sync 콜백 X)

- **장점:** OnAttempt 가 `chan AttemptInfo` 에 send 만 하고 background goroutine 이 consume → gateway latency 에 recorder 의 blocking work 가 영향 X. backpressure 정책 (drop / block / sample) 을 인터페이스 수준에서 명시 가능.
- **단점:** 인터페이스 자체가 비동기라 caller 의 logger / metrics backend 가 sync API 면 wrapping 부담. AsyncWrapper 를 **별 모듈로 제공**하면 사용자가 필요 시 선택 가능 — 인터페이스 자체는 sync 유지.
- **안 선택한 이유:** 인터페이스 강제 비동기는 빠른 backend (Prometheus Counter 등) 의 use case 에서 overhead. CONTRACT godoc + AsyncWrapper 옵션 제공이 trade-off 균형.

### Alt 5 — Q2 Log-only (metric emit 없이 `ChatResponse.Attempts` + slog)

- **장점:** 라이브러리 코어에 metric 추상화 없음. caller 가 Loki / Vector 에서 slog 필드로 필요한 metric 을 직접 derive. ADR-002 의 최소 의존성 원칙에 더 보수적.
- **단점:** 운영 dashboard 의 metric latency가 log pipeline 의 ingestion lag 에 종속 (보통 수 분). real-time alerting 어려움. Prometheus 사용자가 자체 log → metric adapter 작성 부담.
- **안 선택한 이유:** ADR-001 의 "production-grade" 정체성 — real-time alerting 이 production gateway 의 baseline. Recorder 인터페이스 + nil → NoOp 이 log-only 사용자에게도 0-cost (nil 로 두면 됨).

### Alt 6 — Q7 `key_hash` label 포함

- **장점:** per-key 비용 attribution 즉시 가능.
- **단점:** Cardinality 폭발 (사용자 수 = unique combinations). Prometheus / Grafana cost 증가.
- **안 선택한 이유:** 별 metric 으로 분리 (opt-in). Default 셋은 운영 baseline 만.

### Alt 7 — Q5 `slog.Handler` 미들웨어 패턴 (LogRecorder 신설 우회)

대안: `MetricRecorder` 를 구현하는 별 모듈 대신, gateway 가 attempt context (vendor / model / outcome 등) 를 ctx 에 push 하고 caller 가 등록한 custom `slog.Handler` wrapper 가 기존 모든 log record 에 attempt fields 를 자동 inject. Recorder 신설 없이 기존 slog pipeline 에 통합 가능 → "metric 과 log 를 같은 인터페이스에" 끼워맞추는 문제가 사라짐.

**Reject 이유:**
- **단일 발화 지점 보장 불가** — recorder hook 은 gateway 가 `AttemptInfo` 를 구성한 직후 정확히 한 번 호출됨 (attempt 단위 1:1). 미들웨어 패턴은 caller 가 attempt 와 무관한 log call (예: 디버그 출력, 헬스체크) 을 해도 attempt fields 가 inject 되므로 attempt 단위 boundary 가 record 빈도에 매핑 안 됨. dashboard "vendor 별 attempt 수" 같은 baseline 질문조차 record count 로 답 불가.
- **emission 책임의 위치** — 미들웨어는 caller 의 log pipeline 위에 얹는 방식. 그러면 "caller 가 log 안 쓰면 attempt trace 도 사라짐" — gateway 가 자기 record 의 발화 주체여야 (observability 정체성, ADR-001) 한다는 원칙 위반.
- 결과: 인터페이스 conflation 의 비용 (Q5 sub-decision) 을 받아들이는 게 단일 발화 지점 보장 + 발화 책임 owner 측면에서 우위. (참고: AsyncWrapper + ctx staleness 문제는 본 ADR 의 선택지도 동일하게 받음 — Q6 Sub-decision + Open Questions 참고.)

### Alt 8 — Q6 OpenTelemetry Logs API (v0.2 후보)

OTel trace 를 v0.1 에 미룬 이유 (SDK 무거움) 는 타당하나, **OTel Logs API** 는 metric 과 log 를 자연스럽게 분리하며 본 ADR 의 인터페이스 conflation 문제를 해결한다. caller 가 OTel collector 에 metric + log 둘 다 보내는 통합 운영 환경에서는 자연스러운 선택.

**v0.1 reject 이유:**
- OTel Logs SDK 도 trace 와 동일한 weight 부담 (별 ADR-009 OTel 시점에 일괄 land).
- `LogRecorder` 가 별 모듈이므로 OTel Logs adapter 도 같은 `MetricRecorder` 인터페이스 구현으로 future-proof — 본 ADR 의 추상화가 OTel Logs 채택을 막지 않음.
- **v0.2 후보로 명시:** OTel Logs adapter (`pkg/metrics/otellog`) 추가 시 별 ADR-009 또는 OTel 통합 ADR 에서 결정. 본 ADR 의 인터페이스 분리 결정 (`LogRecorder` 별 모듈) 이 v0.2 OTel Logs 추가 부담을 줄임.

---

## Consequences

### Positive

- 운영자가 4가지 정형 질문에 metric 으로 답 (vendor 실패율 / failover / pre-empt 비율 / vendor latency)
- Sentinel 통합 (ADR-005 Q7) 의 정보 손실을 `origin` label 로 보완
- Recorder 인터페이스가 OTel / Datadog 의 future-proof 확장점
- `ChatResponse.Attempts` 가 caller 의 debugging / request_id correlation 직접 노출
- slog 표준 필드가 grep / Loki query 의 일관성 보장

### Negative

- `gateway.Config.Metrics` 추가 필드 → caller 가 nil 또는 NoOp recorder 명시 결정 부담
- `ChatResponse.Attempts` 가 매 호출 시 allocation — 빈 호출이라도 1+ entry 슬라이스. 일반 트래픽에서는 p99 latency 영향 미미 (사전 할당 `cap=len(candidates)` + entry 자체가 ~200 bytes). **단 고트래픽 + 깊은 failover (예: 1k+ RPS × 5+ fallback) 환경에서는 GC pressure 가능** — 그 시점에 `AttemptInfo` pool 재사용 또는 "Attempts 비활성화 옵션" (Config.DisableAttempts) 도입 검토 필요. v0.1 release 시 벤치마크 결과 첨부 예정.
- Recorder hook 콜백이 caller goroutine 에서 실행 — recorder 가 느리면 gateway latency 에 직접 더해진다. 빠른 구현 권장:
  - Prometheus `Counter`: atomic (사실상 lock-free) — OK
  - Prometheus `Histogram.Observe()`: **내부 `sync.Mutex` 사용** — 고트래픽 시 lock 경쟁 발생 가능. 단일 어댑터에서 attemptDuration histogram 호출이 매 attempt 마다 → p99 latency 에 영향. **권장 임계치: ≥ 1k RPS / per Gateway 인스턴스** 이상이면 AsyncWrapper 권장. 이하 트래픽에서는 sync 호출 OK (lock 경쟁 무시 가능). v0.1 release 시 100/1k/10k RPS 벤치마크 결과를 README 에 첨부 예정.
  - OTel remote exporter / Datadog flush 등 IO blocking 구현: 반드시 AsyncWrapper 로 격리 (recorder.go 의 godoc CONTRACT 참고).
  - Recorder 가 내부 goroutine 을 spawn 하면 gateway 의 `recover()` 가 panic 못 잡음 — 그런 recorder 는 자체 recover 필수.

### Risks

| Risk | Mitigation |
|---|---|
| Recorder 가 panic 하면 gateway 가 panic | gateway 내부에서 recover() + slog.Error 로 surface (defensive) |
| Cardinality 폭발 (사용자가 잘못된 model label) | model label 의 화이트리스트 검증 — 등록 안 된 model 은 "unknown" 으로 fallback |
| `Attempts` 필드가 호출자에 의한 mutation | 슬라이스 own — 호출자가 수정해도 라이브러리 내부에 영향 없음 (단, 응답 후 mutation 은 metric 과 불일치) |
| OTel adapter 가 나중에 ctx propagation 룰 충돌 | recorder hook 시그니처에 ctx 포함 → adapter 가 자체 span 추출 가능. 단 AsyncWrapper 통과 시 ctx 는 stale 가능 (Open Question 참고) |
| `outcome` label 의 값 폭발 (vendor 가 새 error code 추가) | ErrorType 9개로 제한, 매핑되지 않으면 "error_unknown" |
| AsyncWrapper Close() race 로 마지막 N event 손실 | CONTRACT 명시 — caller 의 graceful shutdown sequence 책임 (producer 중지 → in-flight 대기 → Close). 본 wrapper 는 zero-loss 보장 X |
| AsyncWrapper + LogRecorder 조합 시 log entry 손실 | metric drop (카운터 손실) 은 통계적으로 tolerable 하지만 log drop 은 post-mortem 의 attempt chain 구멍 — 특히 error 경로 `slog.LevelWarn` 레코드 손실 시 사후 분석 불가. 에러 포렌식이 critical 한 환경에서는 slow handler 라도 sync 유지 (Histogram 처럼 lock 경쟁만 신경) 또는 `DroppedEvents()` alert 구성 (`> 0` 이면 페이지). LogRecorder 전용 AsyncWrapper variant (drop 대신 blocking-with-timeout) 는 v0.2 후보. |
| MultiRecorder 의 한 recorder 가 panic 하면 다른 recorder 도 호출 안 됨 | 개별 recover() 로 격리. Synthesis pseudocode 에 명시. 단 recorder 가 자체 goroutine spawn 후 panic 시는 못 잡음 (Risks 의 일반 recover 한계와 동일) |

---

## Related ADRs

- [ADR-001](0001-why-go-llm-gateway.md) — "production-grade" 정체성 (observability 가 사실상 필수)
- [ADR-003](0003-model-routing-strategy.md) — 라우팅 실패 (no provider supports model) 의 origin=gateway-router 매핑
- [ADR-004](0004-failover-trigger-and-retry.md) — Q5 의 미룬 attempt trace 형식 본 ADR 에서 land
- [ADR-005](0005-distributed-rate-limit.md) — Q7 의 sentinel 통합과 metric origin 분리의 상호작용
- **ADR-008 (예정)** — Cost attribution (per-key USD tracking). 본 ADR 의 `Usage.CostUSD` 가 입력. ADR-007 (streaming) 이 v0.2 의 더 큰 missing 기능으로 우선 land 되어 번호 재할당.

## Open Questions

- [x] **(해결됨)** rate limit backend 에러 시 origin — Q3 Sub-decision 에서 `vendor` 로 결정 (FailOpen path 가 vendor 호출로 이어지므로).
- [ ] Recorder 의 ctx propagation 룰 — OTel adapter 가 등장하면 본 ADR 의 ctx 시그니처가 충분한지 재검토
- [ ] Metric naming convention — Prometheus 의 `_total` / `_seconds` suffix 외에 다른 명명 규칙 (예: `_count`) 필요한지
- [x] **(결정됨)** Log sampling 은 본 ADR 에서 미룸 — `LogRecorder` 가 별 모듈이므로 caller 가 자체 sampling 로직을 wrapping 가능 (예: `SampledRecorder` 를 직접 구현하거나 slog handler 의 sampling 사용). v0.2 에서 `SampledRecorder` 표준 wrapper 도입 여부는 별 ADR. 본 ADR 의 결정: gateway core 는 sampling 책임 없음.
- [ ] `Attempts` 슬라이스 길이 제한 — failover 가 10 vendor 깊이면 메모리 부담. cap 옵션?
- [x] **(결정됨)** `AsyncWrapper` 의 backpressure default 는 **drop newest** (Synthesis pseudocode 의 `select { ... default: dropCounter++ }`). 운영자가 `DroppedEvents()` 로 drop 발생을 모니터링. 다른 정책 (drop oldest / block / sample) 은 별 wrapper 로 caller 가 자체 구현 또는 v0.2 ADR 후보.
- [ ] **1k RPS 임계치는 추정치** — Histogram lock 경쟁의 실제 임계치는 벤치마크 미실시 상태. v0.1 release 시 100/1k/10k RPS 측정으로 확정 또는 정정 예정. 측정 전까지는 운영자가 자체 부하 테스트로 검증 권장.
- [ ] **AsyncWrapper ctx propagation 한계** — channel 에 담긴 ctx 는 consumer 가 꺼낼 때 이미 cancelled 일 수 있음. Prometheus counter / histogram 은 ctx 사용 X 이라 무해, 단 향후 OTel adapter 가 ctx 로 parent span 추출하면 span 유실. recording-only backend 는 context-agnostic 하게 구현 권장. OTel adapter 도입 시 별 ADR 에서 ctx propagation 룰 재검토.
- [ ] `unknown_model_total` 이 PromRecorder 내부 detail — MultiRecorder(prom, logrecorder) 사용 시 log 측에는 unknown model 이벤트 전달 안 됨. 의도된 silent gap. log 에서도 unknown model 추적 필요한 사용자는 LogRecorder 가 자체 known-model set 보유해야 함 (PromRecorder 와 별도).
- [ ] OpenTelemetry 도입 시기 — 본 ADR 의 v0.2 후보를 별 ADR-009 로 분리할지 본 ADR 의 Q6 확장으로 갈지
