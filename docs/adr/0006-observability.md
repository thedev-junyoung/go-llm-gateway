# ADR-006 — Observability Strategy

- **Status:** Proposed *(agent-generated draft, pending maintainer review)*
- **Date:** 2026-06-04
- **Author:** Junyoung
- **비고:** 본 draft 의 결정/근거는 에이전트가 사용자 요청으로 작성. 각 Q 의 `Maintainer note` 줄을 본인 voice 로 채우기 전엔 `Status` 를 `Accepted` 로 바꾸지 말 것.

---

## Context

Gateway 가 multi-vendor failover + per-key rate limit 까지 갖춘 v0.1.0-rc 상태다 (ADR-001~005 완료). 운영자가 "어느 vendor 가 언제 어떤 비율로 실패하나", "사전 차단 vs 실제 vendor 429 비율" 같은 질문을 답할 수단이 아직 없다. 본 ADR 은 그 측정/노출 메커니즘을 결정.

영향 모듈:

- `pkg/metrics` (신규) — metric recorder 인터페이스 + Prometheus 기본 구현
- `pkg/logging` (신규 또는 doc 만) — structured log 필드 표준
- `gateway.Chat` — attempt 단위 측정 hook 추가 (rate limit / vendor 응답 / failover)
- `provider.Provider` — 추가 변경 없음 (기존 Name() / Vendor() 로 label 채움)

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
  - **OpenTelemetry only** — vendor-neutral, future-proof. 그러나 metric API 가 아직 일부 stable (Go SDK 가 spec 보다 늦음), 외부 의존성이 무거움 (~10MB add).
  - **인터페이스 + Prometheus 기본** ✅ — `MetricRecorder` 인터페이스가 base. 호출자는 직접 구현하거나 (예: OTel adapter) 라이브러리가 제공하는 Prometheus 구현 사용. Rate Limit 의 backend pluggable 패턴 (ADR-005 Q1) 과 일관.
- **왜 이 결정이 정당한가:** Prometheus 가 default 인 게 진입성 측면에서 압도적이지만, 한 vendor 에 hard-bind 되면 OpenTelemetry / Datadog 사용자의 진입 장벽. 인터페이스 base + 기본 구현 동시 제공이 ADR-005 의 패턴과 일관.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q2. Metric 셋 (어떤 metric 을 노출하나)

- **Decision:** **Counter 2 + Histogram 1** — 최소 셋:

  | metric | type | labels | 의미 |
  |---|---|---|---|
  | `llm_gateway_requests_total` | counter | `vendor`, `model`, `outcome`, `origin` | 모든 attempt 의 결과 (vendor 별 분해) |
  | `llm_gateway_failovers_total` | counter | `from_vendor`, `to_vendor`, `reason` | failover trigger 발생 횟수 (cascading 분석) |
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
- **Sub-decision (ADR-005 fail-open path):** rate limit backend 가 에러 → FailOpen default → vendor 호출 진행. 이 경로의 origin 은 `vendor` (vendor 가 응답하므로). backend 에러 자체는 별 metric (예: `llm_gateway_ratelimit_errors_total`) 으로 분리하거나 slog.Warn 으로만 surface — origin label 의 4번째 state 도입은 사용자 mental model 만 복잡하게 함.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q4. Attempt trace 노출 방식 (ADR-004 Q5 의 미룬 결정)

- **Decision:** B — **`ChatResponse.Attempts []AttemptInfo` 필드 + recorder hook 동시**.
  - `ChatResponse.Attempts` — caller 가 응답에서 직접 읽음 (예: debugging log)
  - `MetricRecorder.OnAttempt(info AttemptInfo)` — metric / log integration 의 single source of truth
- **Agent reasoning:** 세 가지 옵션 비교:
  - **A. Response 필드만** — caller 가 직접 처리. 메트릭 집계는 caller 책임. 메트릭 라이브러리 의존성 없음. 단점: 매 caller 마다 metric 코드 중복.
  - **B. Recorder hook 만** — gateway 내부 metric recorder 가 attempt 마다 콜백. caller 는 모름. 단점: caller 가 trace 정보 필요할 때 (response logging, request_id correlation) 별도 channel 필요.
  - **C. 둘 다** ✅ — Response 필드는 caller-facing, Recorder hook 은 metric/log facing. 의도가 명확.
- **왜 이 결정이 정당한가:** ADR-004 가 미룬 이유 (metric 모듈의 요구 사항이 입력) 가 이제 명확해짐 — recorder 패턴이 metric/log 의 공통 sink, response 필드가 caller 의 debugging 용. 두 path 가 서로 직교.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q5. Structured logging 표준

- **Decision:** **`log/slog` 표준 필드 명시 + recorder hook 에서 자동 emit**.
  - 표준 필드: `request_id`, `vendor`, `model`, `attempt`, `outcome`, `duration_ms`, `origin`
  - `MetricRecorder.OnAttempt` 가 위 필드로 slog.Info / Warn 호출 (recorder 구현이 책임)
- **Agent reasoning:** Go 1.21+ 의 `log/slog` 가 사실상 표준이 됐고, gateway 내부에서 이미 ADR-005 fail-open 시 `slog.WarnContext` 사용. caller 가 자체 logger 를 쓰면 `slog.Default()` 를 자기 logger 로 바꾸면 됨 (호출 측 책임). 라이브러리는 *어떤 필드를* emit 할지만 정함.
- **왜 이 결정이 정당한가:** slog 가 stdlib 으로 들어와서 호환성 부담 0. 필드 표준화가 grep/Loki query 의 일관성을 보장. logger 인스턴스 자체는 caller 가 control.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q6. Distributed tracing (OpenTelemetry spans)

- **Decision:** **v0.1 에 포함 X** — Recorder 인터페이스가 향후 OTel adapter 의 확장점.
- **Agent reasoning:** OTel span 도입은 매력적이지만:
  - 외부 의존성 무겁다 (otel-go SDK + propagators 합치면 +5MB)
  - context propagation 룰이 OTel spec 종속 (예: span 이 ctx 에 어떻게 carried 되는지)
  - 사용자가 OTel 안 쓰면 dead weight
  - 본 Recorder 인터페이스가 OnAttempt 콜백을 제공하므로, OTel adapter 가 별 모듈 (`pkg/metrics/otel`) 로 적층 가능 — 라이브러리 코어에 강제 의존성 X.
- **왜 이 결정이 정당한가:** YAGNI. OTel 수요가 실제 발생할 때 별 ADR 로 도입 (v0.2 후보). 본 ADR 은 Recorder 추상화로 future-proof.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q7. Cardinality 폭발 방지

- **Decision:** **`key_hash` 는 metric label 에서 제외, `model` 은 표준 모델명만 허용**.
  - `vendor` label — provider.Name() (예: "openai", "anthropic") — 카디널리티 ≤ 10
  - `model` label — vendor 의 표준 model id ("gpt-4o", "claude-opus-4-7" 등) — 카디널리티 ≤ 50
  - `outcome` label — `success` / `error_<type>` (ErrorType 9개) — 카디널리티 ≤ 10
  - `origin` label — 3-state — 카디널리티 = 3
  - `key_hash` 는 라벨 X — 비용 attribution 이 필요하면 별 metric (예: `llm_gateway_cost_usd_by_key_total`) 로 분리, 본 ADR 에서는 미포함.
- **Agent reasoning:** Prometheus 의 cardinality 한도 (보통 ≤ 100k unique label combinations per metric) 를 안전하게 유지. `key_hash` 를 label 로 넣으면 사용자 수만큼 카디널리티 폭발 → Prometheus storage cost / Grafana query latency 증가. 비용 attribution 은 별 use case 라 별 metric 으로 분리.
- **왜 이 결정이 정당한가:** Prometheus best practice ("don't put high-cardinality data in labels"). per-key 비용 attribution 이 critical 한 사용자는 별 metric 으로 opt-in. 기본 셋은 운영 dashboard 의 baseline 만 cover.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

---

### Synthesis — Go pseudocode

```go
// 의존성 방향 (순환 방지):
//   - AttemptInfo / FailoverInfo / Outcome / Origin 은 pkg/provider 에 정의.
//     ChatResponse.Attempts 가 이 타입을 참조해도 provider → metrics 의존성 X.
//   - pkg/metrics 는 provider 를 import 해서 위 타입을 사용. provider 는 metrics 를
//     import 하지 않음. 일방향.

// --- pkg/provider ---

package provider

import "time"

// AttemptInfo 는 한 vendor 시도의 결과. ChatResponse.Attempts 에 들어가는 entry.
type AttemptInfo struct {
	Vendor   string         // Provider.Name()
	Model    string         // ChatRequest.Model
	AttemptN int            // 0=primary, 1+=fallback
	Outcome  Outcome        // success / error_<type>
	Origin   Origin         // vendor / gateway-preempt / gateway-router
	Duration time.Duration
	Usage    Usage          // 성공 시
	Error    *ProviderError // 실패 시
}

type FailoverInfo struct {
	FromVendor string
	ToVendor   string
	Reason     ErrorType
}

type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	// ErrorType 별 outcome (ErrorTypeRateLimit → "error_rate_limit" 등) — 자동 매핑
)

type Origin string

const (
	OriginVendor         Origin = "vendor"          // provider 가 응답 (성공 / vendor-side 에러)
	OriginGatewayPreempt Origin = "gateway-preempt" // rate limit pre-check 차단
	OriginGatewayRouter  Origin = "gateway-router"  // router 라우팅 실패
)

// --- pkg/metrics ---

package metrics

import (
	"context"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// MetricRecorder 는 gateway 가 매 attempt 마다 호출하는 콜백 인터페이스. caller 는
// 자체 backend (Prometheus / OTel / Datadog / no-op) 의 어댑터를 구현해 주입.
type MetricRecorder interface {
	OnAttempt(ctx context.Context, info provider.AttemptInfo)
	OnFailover(ctx context.Context, info provider.FailoverInfo)
}

// NoOpRecorder 는 default — Gateway 가 Config.Metrics 가 nil 일 때 사용.
type NoOpRecorder struct{}

func (NoOpRecorder) OnAttempt(context.Context, provider.AttemptInfo)   {}
func (NoOpRecorder) OnFailover(context.Context, provider.FailoverInfo) {}

// gateway.Config 확장 — non-breaking.
type Config struct {
	Providers []provider.Provider
	RateLimit ratelimit.RateLimiter
	Metrics   metrics.MetricRecorder // 새로 추가, nil 이면 New 가 NoOpRecorder{} 로 대체
}

// gateway.New 의 nil → NoOp 대체 (Chat pseudocode 의 nil-check 부담 제거).
func New(cfg Config) (*Gateway, error) {
	// ... 기존 validation ...
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.NoOpRecorder{}
	}
	return &Gateway{providers: cfg.Providers, rateLimit: cfg.RateLimit, metrics: cfg.Metrics}, nil
}

// provider.ChatResponse 확장 — non-breaking 필드 추가.
type ChatResponse struct {
	Content      string
	FinishReason FinishReason
	Usage        Usage
	Raw          []byte
	Attempts     []provider.AttemptInfo // 새로 추가, 호출자의 debugging 용
}
```

**Prometheus 기본 구현 (별 모듈):**

```go
package promrecorder

import "github.com/prometheus/client_golang/prometheus"

func New() *PromRecorder { /* counter / histogram 등록 */ }

func (r *PromRecorder) OnAttempt(ctx context.Context, info provider.AttemptInfo) {
	r.requestsTotal.WithLabelValues(info.Vendor, info.Model, string(info.Outcome), string(info.Origin)).Inc()
	r.attemptDuration.WithLabelValues(info.Vendor, info.Model, string(info.Outcome)).Observe(info.Duration.Seconds())
}

func (r *PromRecorder) OnFailover(ctx context.Context, info provider.FailoverInfo) {
	r.failoversTotal.WithLabelValues(info.FromVendor, info.ToVendor, string(info.Reason)).Inc()
}
```

**gateway.Chat 통합:**

```go
// recordAttempt 는 OnAttempt 콜백을 호출하면서 recorder 의 panic 을 격리 (Risks 표).
func (g *Gateway) recordAttempt(ctx context.Context, info provider.AttemptInfo) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "metrics.OnAttempt panicked", "panic", r, "vendor", info.Vendor)
		}
	}()
	g.metrics.OnAttempt(ctx, info)
}

func (g *Gateway) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	primary, fallbacks, err := router.PickWithFallbacks(g.providers, req.Model)
	if err != nil {
		// origin=gateway-router. Vendor 가 없으므로 빈 문자열, AttemptN=0.
		g.recordAttempt(ctx, provider.AttemptInfo{
			Model: req.Model, Outcome: outcomeFromErr(err),
			Origin: provider.OriginGatewayRouter, Error: asProviderError(err),
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
					Outcome: "error_rate_limit", Origin: provider.OriginGatewayPreempt,
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
			Duration: time.Since(start), Origin: provider.OriginVendor,
		}
		if cerr == nil {
			info.Outcome = provider.OutcomeSuccess
			info.Usage = resp.Usage
			attempts = append(attempts, info)
			g.recordAttempt(ctx, info)
			resp.Attempts = attempts
			return resp, nil
		}
		info.Outcome = outcomeFromErr(cerr)
		info.Error = asProviderError(cerr)
		attempts = append(attempts, info)
		g.recordAttempt(ctx, info)

		if !shouldFailover(cerr) { /* abort */ }
		if n+1 < len(candidates) {
			g.metrics.OnFailover(ctx, provider.FailoverInfo{
				FromVendor: p.Name(), ToVendor: candidates[n+1].Name(), Reason: info.Error.Type,
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

### Alt 4 — Q7 `key_hash` label 포함

- **장점:** per-key 비용 attribution 즉시 가능.
- **단점:** Cardinality 폭발 (사용자 수 = unique combinations). Prometheus / Grafana cost 증가.
- **안 선택한 이유:** 별 metric 으로 분리 (opt-in). Default 셋은 운영 baseline 만.

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
- `ChatResponse.Attempts` 가 매 호출 시 allocation — 빈 호출이라도 1+ entry 슬라이스. p99 latency 에 미미한 영향
- Recorder hook 콜백이 caller goroutine 에서 실행 — recorder 가 느리면 gateway latency 에 영향. 빠른 구현 권장 (Prometheus 는 lock-free 카운터)

### Risks

| Risk | Mitigation |
|---|---|
| Recorder 가 panic 하면 gateway 가 panic | gateway 내부에서 recover() + slog.Error 로 surface (defensive) |
| Cardinality 폭발 (사용자가 잘못된 model label) | model label 의 화이트리스트 검증 — 등록 안 된 model 은 "unknown" 으로 fallback |
| `Attempts` 필드가 호출자에 의한 mutation | 슬라이스 own — 호출자가 수정해도 라이브러리 내부에 영향 없음 (단, 응답 후 mutation 은 metric 과 불일치) |
| OTel adapter 가 나중에 ctx propagation 룰 충돌 | recorder hook 시그니처에 ctx 포함 → adapter 가 자체 span 추출 가능 |
| `outcome` label 의 값 폭발 (vendor 가 새 error code 추가) | ErrorType 9개로 제한, 매핑되지 않으면 "error_unknown" |

---

## Related ADRs

- [ADR-001](0001-why-go-llm-gateway.md) — "production-grade" 정체성 (observability 가 사실상 필수)
- [ADR-003](0003-model-routing-strategy.md) — 라우팅 실패 (no provider supports model) 의 origin=gateway-router 매핑
- [ADR-004](0004-failover-trigger-and-retry.md) — Q5 의 미룬 attempt trace 형식 본 ADR 에서 land
- [ADR-005](0005-distributed-rate-limit.md) — Q7 의 sentinel 통합과 metric origin 분리의 상호작용
- **ADR-007 (예정)** — Cost attribution (per-key USD tracking). 본 ADR 의 `Usage.CostUSD` 가 입력.

## Open Questions

- [x] **(해결됨)** rate limit backend 에러 시 origin — Q3 Sub-decision 에서 `vendor` 로 결정 (FailOpen path 가 vendor 호출로 이어지므로).
- [ ] Recorder 의 ctx propagation 룰 — OTel adapter 가 등장하면 본 ADR 의 ctx 시그니처가 충분한지 재검토
- [ ] Metric naming convention — Prometheus 의 `_total` / `_seconds` suffix 외에 다른 명명 규칙 (예: `_count`) 필요한지
- [ ] Log sampling — 트래픽 폭증 시 OnAttempt 의 매 호출 slog.Info 가 부담. sampling 옵션을 본 ADR 에서 land 할지 별 ADR 로 미룰지
- [ ] `Attempts` 슬라이스 길이 제한 — failover 가 10 vendor 깊이면 메모리 부담. cap 옵션?
- [ ] OpenTelemetry 도입 시기 — 본 ADR 의 v0.2 후보를 별 ADR-008 로 분리할지 본 ADR 의 Q6 확장으로 갈지
