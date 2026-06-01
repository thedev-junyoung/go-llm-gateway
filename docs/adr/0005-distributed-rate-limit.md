# ADR-005 — Distributed Rate Limit Strategy

- **Status:** Proposed *(agent-generated draft, pending maintainer review)*
- **Date:** 2026-06-01
- **Author:** Junyoung
- **비고:** 본 draft 의 결정/근거는 에이전트가 사용자 요청으로 작성. 각 Q 의 `Maintainer note` 줄을 본인 voice 로 채우기 전엔 `Status` 를 `Accepted` 로 바꾸지 말 것.

---

## Context

LLM API key 는 vendor 마다 RPM (Requests Per Minute) + TPM (Tokens Per Minute) 한도가 있다. 같은 key 를 N개 인스턴스가 공유하면 각자 N분의 1만큼 모르고 호출 → 합쳐서 한도 초과 → vendor 가 429 폭격. 이때 발생하는 결과:

1. failover 가 발동하지만 다른 vendor 도 같은 패턴이면 cascading throttle
2. caller 가 retry 하면 backoff 안 한 채 같은 429 반복 → snowball
3. 비용/지연/품질 모두 손상

본 ADR 은 *vendor 가 거부하기 전에 gateway 가 사전 차단* 하는 메커니즘을 결정. 영향 모듈:

- `pkg/ratelimit` (신규) — 측정/판정 인터페이스 + Redis 기본 구현
- `gateway.Chat` — Chat() 진입부에 ratelimit 호출 추가
- `gateway.Config` — `RateLimit RateLimiter` 옵션 필드 (non-breaking 추가)

추가 제약:
- ADR-001 의 "production-grade" 정체성 — multi-instance 동작이 v0.1 기본 가정
- ADR-002 의 `ErrRateLimited` sentinel — 사전 차단도 같은 sentinel 사용 (call site 통일)
- ADR-004 의 failover 와 의미 충돌 없어야 — sentinel 통일이 그 보장

---

## Decision

### Q1. Backend

- **Decision:** C — **Pluggable `RateLimiter` interface + v0.1 에 in-memory 와 Redis 두 구현 동시 제공**.
- **Agent reasoning:** Multi-instance 가 v0.1 기본이지만 (Redis 필요), 단일 인스턴스 dev/test 환경에서 Redis 의존은 부담 — in-memory impl 이 있으면 `examples/basic` 도 zero-deps 로 가능. Interface 가 base 라 v0.2 에서 Memcached / Postgres / Cloudflare KV 등 추가 자유. 구현 두 개 동시 land 가 interface 설계의 검증.
- **왜 이 결정이 정당한가:** "Redis only" 면 single-instance 사용자가 라이브러리 진입 못 함. "in-memory only" 면 multi-instance 정확성 보장 안 됨 → 본 라이브러리 핵심 가치 손상. Interface 가 둘을 통합하는 게 architecture rule (provider 가 그렇듯) 의 일관된 적용.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q2. 알고리즘

- **Decision:** **Sliding window log** (Redis 의 경우 SortedSet + Lua script 로 원자성).
- **Agent reasoning:** 네 가지 후보 비교:
  - **Sliding window log** — 요청마다 timestamp 를 SortedSet 에 ZADD, window 밖 entry 는 ZREMRANGEBYSCORE 로 pruning. 메모리는 요청 수에 비례하나 LLM 트래픽 (분당 수백~수천 요청 / key) 에서 충분히 허용 (각 entry ~30 bytes × 수천 = 100KB 미만). Atomic Lua 가 가장 단순.
  - **Sliding window counter** — log 의 메모리를 bucket (예: 1초) 단위로 HINCRBY 로 압축. LLM 운영 트래픽에서는 log 의 메모리 부담이 작아 counter 의 복잡성을 정당화 못 함. window edge 가 bucket 경계에서 약간 부정확.
  - **Token bucket** — burst 허용. 그러나 vendor 한도는 strict 한 sliding window (예: OpenAI "RPM in any 60-second window") → token bucket 의 burst 가 vendor 429 trigger.
  - **Fixed window** — 구현 단순하지만 window 경계에서 2× 한도 트래픽 가능 (notorious flaw). vendor 가 즉시 429.
- **왜 이 결정이 정당한가:** vendor 의 실제 측정 방식에 가장 가까운 알고리즘이 sliding window. LLM 트래픽 규모에서 log 의 메모리는 부담 아님 — counter 의 복잡성 (bucket sum 로직, 경계 부정확성) 을 도입할 동기가 약함. Token bucket 의 burst 는 vendor 한도에서 정당화 불가.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q3. 측정 시점 (entry vs after-response)

- **Decision:** C — **둘 다: request entry 예측치 + after-response 실측 보정**.
- **Agent reasoning:** TPM 측정은 본질적으로 *사후 정보* — 응답이 와야 OutputTokens 안다. 그러나 entry 시점에 아무것도 안 세면 burst 시 카운터가 빈 채로 N개 요청 동시 통과 → 모두 vendor 한도 초과 후 429.
  - Entry: InputTokens 추정 (사용자 message 의 char 수 / 4 같은 휴리스틱) + max_tokens 를 OutputTokens 상한으로 가정 → 보수적 예약
  - After-response: 실제 `Usage.InputTokens + Usage.OutputTokens` 로 카운터 보정 (예약했던 보수적 값과 차이만큼 환불)
  - 두 단계 모두 같은 sliding window 키에 ADD — Redis Lua 한 트랜잭션
- **왜 이 결정이 정당한가:** Entry-only 면 burst 통과 → vendor 429 → ADR-005 무력화. After-only 면 카운터가 빈 상태로 모두 통과 → 같은 결과. 두 시점 결합이 정확성+가용성 균형. *보수적 예약 + 사후 환불* 패턴은 분산 시스템 정석 (Redis distributed lock 와 같은 정신).
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q4. RPM vs TPM

- **Decision:** C — **둘 다 (각각 별도 한도, 어느 한쪽이라도 초과 시 차단)**.
- **Agent reasoning:** Vendor 의 실제 한도가 둘 다임 (OpenAI/Anthropic 모두). RPM 만 검사하면 토큰이 큰 요청 하나가 TPM 초과 가능; TPM 만 검사하면 가벼운 요청 폭주 시 RPM 초과 가능. 한도가 OR 조건이라 검사도 OR.
- **왜 이 결정이 정당한가:** Vendor 의 한도 모양을 그대로 따르는 게 안전. "RPM 만" 같은 단순화는 vendor 의 미래 RPM/TPM 비율 변경에 취약. Config 에서 한쪽을 비활성화 (값 0) 할 수 있도록 유지.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q5. Granularity (어느 단위로 카운트)

- **Decision:** **per-provider × per-key** (key 모양: `ratelimit:{provider_name}:{key_hash}:{rpm|tpm}:{window}`).
- **Agent reasoning:** Vendor 한도가 *API key 단위* 라 그게 가장 정확. Provider 이름 prefix 로 OpenAI/Anthropic 키 가 같은 hash 우연 일치 시에도 분리. key_hash 는 SHA256 (key 원문은 Redis 에 안 저장 — 보안).
  - Per-model 까지 쪼개기: vendor 한도는 model 별이 아님 (대부분). 쪼개면 한 model 의 burst 가 다른 model 를 차단 → 위 결정과 다르게 동작
  - Per-tenant: application layer 의 책임 (이 라이브러리는 vendor 한도 보호; 사용자 쿼터는 사용자 코드의 일)
- **왜 이 결정이 정당한가:** Vendor 가 측정하는 단위에 1:1 매칭이 가장 정확. 더 잘게 쪼개면 vendor 한도와 misalign — gateway 가 OK 했는데 vendor 가 429.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q6. 실패 모드 (Redis 다운 시)

- **Decision:** **Fail-open (allow) default, Config 로 fail-closed (deny) 선택 가능**.
- **Agent reasoning:** Rate limit 자체가 *vendor 보호* 가 목적이지 사용자 차단이 목적이 아님. Redis 가 다운된 경우:
  - Fail-open: 사용자 요청은 통과, vendor 한도 도달 시 vendor 가 429 반환 → ADR-004 failover trigger. 가용성 유지.
  - Fail-closed: 사용자 요청 즉시 거부 → Redis 장애가 service outage 로 즉시 확산.
  - Fail-open 의 비용: Redis 복구까지 vendor 한도 초과 가능성. 그러나 vendor 의 429 가 backup safety net.
  - Fail-closed 가 정당한 케이스: 비용 제한이 critical 한 운영 (예: 무료 트라이얼 사용자) — Config 로 opt-in.
- **왜 이 결정이 정당한가:** *가용성 > 정확성* 의 default 가 production gateway 의 일반 원칙. fail-closed 는 명시적 선택일 때만 (사용자 의도). Library 가 silent 하게 deny 시작하면 운영자 디버깅 시간 폭증.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q7. Failover (ADR-004) 와의 상호작용

- **Decision:** **Vendor 429 도 router failover trigger 그대로, 사전 차단은 같은 sentinel `ErrRateLimited` 반환**.
- **Agent reasoning:** 두 path 가 의미적으로 같음 ("이 vendor 의 한도 초과") — call site 가 같은 sentinel 로 처리해야 일관성 유지.
  - 사전 차단 (Q3 의 entry check): `*ProviderError{Type: ErrorTypeRateLimit, Retriable: true, Vendor: providerName}` 반환 → gateway loop 의 shouldFailover 가 true → 다음 provider 시도
  - Vendor 429: 어댑터의 mapHTTPError 가 이미 같은 형태로 매핑 (ADR-002 + 어댑터 구현)
  - 두 path 의 차이는 *어디서 발생했는가* 만 — sentinel 자체는 동일
- **왜 이 결정이 정당한가:** 사용자 코드에 두 가지 rate-limit error 가 떠다니면 (`errors.Is(err, ErrRateLimited)` 가 한 케이스는 true 한 케이스는 false) 디버깅 지옥. 사전 차단도 vendor 한도 의미라 같은 sentinel 이 정확. *origin 차이는 metric/log 에서 분류* (Q4 ADR-006 추가 정보).
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

---

### Synthesis — Go pseudocode

```go
package ratelimit

import (
	"context"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// RateLimiter is the v0.1 contract every backend implements. Backends MUST
// honor the per-provider × per-key granularity defined in ADR-005 Q5.
type RateLimiter interface {
	// Allow asks whether `req` may proceed under the current window. Called
	// at gateway.Chat entry. If allowed, the limiter reserves a conservative
	// estimate (req + max_tokens) against the window — the response is
	// reconciled via Record after the vendor call.
	Allow(ctx context.Context, providerName, apiKeyHash string, req provider.ChatRequest) (Decision, error)

	// Record reconciles the reservation with actual Usage from the vendor.
	// Called after a successful Chat. Implementations MAY refund the
	// (reservation - actual) difference.
	Record(ctx context.Context, providerName, apiKeyHash string, usage provider.Usage) error
}

// Decision carries the limiter's verdict plus a Retry-After hint when denied.
type Decision struct {
	Allow      bool
	RetryAfter *time.Duration // populated when Allow==false; nil otherwise
}

// Limits configures per-provider quotas. Zero means "unlimited" for that
// dimension — set to 0 to disable RPM-only or TPM-only checks (ADR-005 Q4).
type Limits struct {
	RequestsPerMinute int // RPM
	TokensPerMinute   int // TPM (input + output combined, per ADR-005 Q4)
}

// Options shared by all backend constructors.
type Options struct {
	Limits     Limits
	FailMode   FailMode // ADR-005 Q6
	Clock      func() time.Time // injectable for tests
}

type FailMode int
const (
	FailOpen   FailMode = iota // default — allow on backend errors (ADR-005 Q6)
	FailClosed                  // opt-in — deny on backend errors
)
```

**In-memory backend** (single instance, no external deps, dev/test):

```go
package ratelimit

func NewMemory(opts Options) *MemoryBackend { /* sliding window counter in process */ }
var _ RateLimiter = (*MemoryBackend)(nil)
```

**Redis backend** (multi-instance, production):

```go
package ratelimit

import "github.com/redis/go-redis/v9"

func NewRedis(client redis.UniversalClient, opts Options) *RedisBackend { /* SortedSet + Lua */ }
var _ RateLimiter = (*RedisBackend)(nil)
```

Lua script (atomic; runs on each `Allow`):

```lua
-- KEYS[1] = ratelimit:{provider}:{key_hash}:rpm
-- KEYS[2] = ratelimit:{provider}:{key_hash}:tpm
-- ARGV[1] = now              (unix seconds, integer)
-- ARGV[2] = rpm_limit        (integer)
-- ARGV[3] = tpm_limit        (integer)
-- ARGV[4] = estimated_tokens (input estimate + max_tokens, integer)
-- ARGV[5] = window_seconds   (60)
-- ARGV[6] = nonce            (per-request UUID supplied by caller — prevents member collision)

local now    = tonumber(ARGV[1])
local cutoff = now - tonumber(ARGV[5])
local tokens = tonumber(ARGV[4])
local nonce  = ARGV[6]

-- Prune entries older than the window.
redis.call('ZREMRANGEBYSCORE', KEYS[1], 0, cutoff)
redis.call('ZREMRANGEBYSCORE', KEYS[2], 0, cutoff)

-- RPM: one member per request, score=timestamp. ZCARD = requests in window.
local rpm_used = redis.call('ZCARD', KEYS[1])

-- TPM: score=timestamp (used only for window pruning).
--      member="<tokens>:<nonce>" — we decode tokens back from the member.
--      (Using score as the token count would collide with window pruning,
--       and using member as just "<tokens>" would dedup same-token requests.)
local tpm_entries = redis.call('ZRANGEBYSCORE', KEYS[2], cutoff, '+inf')
local tpm_used = 0
for i = 1, #tpm_entries do
  local member = tpm_entries[i]
  local sep = string.find(member, ':')
  if sep then tpm_used = tpm_used + tonumber(string.sub(member, 1, sep - 1)) end
end

if rpm_used + 1      > tonumber(ARGV[2]) then return {0, 'rpm'} end
if tpm_used + tokens > tonumber(ARGV[3]) then return {0, 'tpm'} end

-- Reserve. Nonce makes members unique so concurrent same-second / same-token
-- requests don't deduplicate in the ZSET.
redis.call('ZADD', KEYS[1], now, tostring(now) .. ':' .. nonce)
redis.call('ZADD', KEYS[2], now, tostring(tokens) .. ':' .. nonce)
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[5]))
redis.call('EXPIRE', KEYS[2], tonumber(ARGV[5]))
return {1}
```

The Go caller generates a UUID per `Allow` and passes it as `ARGV[6]`. The
nonce is opaque to Redis — only used to keep ZSET members unique.

**gateway.Chat 통합** (ADR-004 의 loop 안에서):

```go
func (g *Gateway) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	primary, fallbacks, err := router.PickWithFallbacks(g.providers, req.Model)
	if err != nil { return provider.ChatResponse{}, err }

	candidates := append([]provider.Provider{primary}, fallbacks...)
	var lastErr error
	for _, p := range candidates {
		if cerr := ctx.Err(); cerr != nil { /* dual-wrap as ADR-004 */ }

		// NEW: pre-check (ADR-005 Q3 entry side)
		if g.rateLimiter != nil {
			d, lerr := g.rateLimiter.Allow(ctx, p.Name(), apiKeyHashOf(p), req)
			if lerr != nil {
				// ADR-005 Q6: fail-open default — log and proceed
				slog.Warn("ratelimit backend error", "err", lerr, "vendor", p.Name())
			} else if !d.Allow {
				// Same sentinel as vendor 429 — ADR-005 Q7
				pe := provider.NewProviderError(p.Name(), provider.ErrorTypeRateLimit, 0, true,
					"pre-empted by gateway rate limiter", nil)
				if d.RetryAfter != nil { pe = pe.WithRetryAfter(*d.RetryAfter) }
				lastErr = pe
				continue // try next candidate (failover)
			}
		}

		resp, cerr := p.Chat(ctx, req)
		if cerr == nil {
			// NEW: reconcile (ADR-005 Q3 after-response side)
			if g.rateLimiter != nil {
				_ = g.rateLimiter.Record(ctx, p.Name(), apiKeyHashOf(p), resp.Usage)
			}
			return resp, nil
		}
		lastErr = cerr
		if !shouldFailover(cerr) { return provider.ChatResponse{}, cerr }
	}
	return provider.ChatResponse{}, lastErr
}
```

`apiKeyHashOf(p)` 는 provider 인터페이스 확장 후보 (다음 ADR-006 input).

---

## Alternatives Considered

### Alt 1 — Q1 Redis-only

- **장점:** Single backend → 인터페이스 단순. Multi-instance 정확성 보장.
- **단점:** Single-instance / dev / test 환경에서 Redis 의존이 진입 장벽. CI 도 Redis docker-compose 필요. `examples/basic` 같은 zero-deps demo 불가.
- **안 선택한 이유:** 라이브러리 진입성. interface base 가 추가 비용 작음 (in-memory impl 도 sliding window counter 한 번 작성).

### Alt 2 — Q2 Token bucket

- **장점:** Burst 허용 → 사용자 친화적 (gateway 가 갑작스러운 요청 흡수).
- **단점:** Vendor 한도가 strict sliding window → token bucket 의 burst 가 정확히 vendor 429 trigger. 사용자가 "gateway 가 OK 했는데 vendor 가 거부" 디버깅.
- **안 선택한 이유:** Vendor 의 한도 모양을 따르는 게 안전. Burst 허용은 application layer 의 throttle 책임.

### Alt 3 — Q6 Fail-closed default

- **장점:** Cost protection 강함. Redis 다운 시 비용 폭주 방지.
- **단점:** Redis 장애 → 즉시 service outage. operational risk ↑.
- **안 선택한 이유:** 가용성 우선의 production gateway 정체성. 비용 critical 사용자는 Config 로 opt-in.

### Alt 4 — Q5 Per-model granularity

- **장점:** 같은 key 의 GPT-4 와 GPT-3.5 한도 분리 가능 → 더 세밀한 cost control.
- **단점:** Vendor 한도가 model 별이 아님 → gateway 의 model 별 카운트 와 vendor 의 key 별 카운트 misalign. Gateway OK → vendor 429 시나리오.
- **안 선택한 이유:** Vendor 한도 모양을 따라야 함 (Q5 안 선택 이유 와 동일).

---

## Consequences

### Positive

- Vendor 한도 도달 전 사전 차단 → 429 폭격 회피 → 비용/지연/품질 보호
- 사전 차단과 vendor 429 가 같은 sentinel → 사용자 코드 일관
- In-memory backend 로 zero-deps dev/test (Redis 옵션)
- Fail-open default 로 Redis 장애 ≠ service outage
- 사전 예약 + 사후 보정 패턴 → burst 통과 + 정확성 둘 다

### Negative

- Limiter 호출이 hot path (Chat 마다 Redis round-trip 1번 + Lua 실행). 지연 ~1-2ms 추가. p99 latency 가 critical 한 경우 영향
- API key 가 Provider 인터페이스에 없음 — `apiKeyHashOf(p)` 같은 helper 추가 필요 (provider 인터페이스 확장 또는 별도 accessor) — 어댑터에서 노출 결정 필요
- Token estimation (entry 시점) 이 정확하지 않음 (사용자 message 의 실제 토큰화 결과와 char/4 휴리스틱 차이) → 가끔 vendor 429 통과 (보수적 예약이 over-estimate 면 over-block 도 가능)
- 사전 예약과 사후 보정 사이에 vendor 호출이 실패하면 예약 환불 누락 → 카운터가 over-count (가용성 감소). v0.1 에서는 약간의 over-count 허용 (다음 window 에서 자동 해소)

### Risks

| Risk | Mitigation |
|---|---|
| Redis 다운 → fail-open default 라 카운트 안 됨 → vendor 429 폭격 | doc 에 "Redis HA / sentinel 권장" 명시 + fail-closed opt-in 선택지 |
| Token estimation 정확도 낮음 → 예약이 실제와 크게 다름 | After-response Record 가 보정. 큰 차이는 다음 window 에서 자체 해소 |
| 두 인스턴스가 거의 동시에 Allow 호출 → 둘 다 통과 후 vendor 429 | Lua script 가 원자 → 같은 Redis 면 timing 문제 없음. 다른 Redis cluster 에 분산하면 atomicity 깨짐 — single Redis cluster 권장 |
| Provider 인터페이스에 API key 노출 → 추상화 깨짐 가능 | Provider 가 `KeyHash()` 만 노출 (전체 key 가 아닌 hash) — 별도 follow-up issue 로 트래킹 |
| sliding window counter 의 bucket size (1초) 가 너무 coarse → 1초 burst 100% 가능 | 한도 운영상 1초 정도 burst 는 vendor 가 보통 허용 (RPM 은 60초 평균). 정밀도 필요 시 v0.2 ADR |

---

## Related ADRs

- [ADR-001](0001-why-go-llm-gateway.md) — "distributed rate limit" 이 분산락 학습 강점의 정면 활용 (본 ADR 의 존재 이유)
- [ADR-002](0002-provider-interface-design.md) — `ErrRateLimited` sentinel + `RetryAfter` 필드가 본 ADR Q7 의 직접 입력
- [ADR-004](0004-failover-trigger-and-retry.md) — failover loop 가 본 ADR 의 entry-check 와 vendor 429 둘 다 처리하는 무대
- **ADR-006 (예정)** — observability. 본 ADR 의 entry-check 차단 vs vendor 429 origin 구분이 metric label 의 입력

## Open Questions

- [ ] Provider 인터페이스에 `KeyHash() string` 추가 vs 별도 `RateLimitable` optional interface (architecture base-layer rule 의 적용 결정)
- [ ] Token estimation 알고리즘 — char/4 휴리스틱 vs tiktoken/cl100k_base library 의존 (정확도 vs 의존성 트레이드오프)
- [ ] Burst 허용을 위한 별도 short-window check (1초/10초) — v0.2 에서 사용자 피드백 받고
- [ ] Sliding window counter 의 bucket 크기 조정 가능 옵션 (현재 1초 fixed) — 정밀도 vs 메모리
- [ ] Cost-based throttling (USD 한도) — v0.2 별도 ADR. 본 ADR 은 RPM/TPM 만
- [ ] Per-tenant (downstream user) 한도 — application layer 책임으로 본 ADR 에서 명시 제외, 사용자 doc 에서 가이드
