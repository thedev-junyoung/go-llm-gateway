# ADR-007 — Streaming Responses

- **Status:** Proposed *(agent-generated draft, pending maintainer review)*
- **Date:** 2026-06-04
- **Author:** Junyoung
- **비고:** 본 draft 의 결정/근거는 에이전트가 사용자 요청으로 작성. 각 Q 의 `Maintainer note` 줄을 본인 voice 로 채우기 전엔 `Status` 를 `Accepted` 로 바꾸지 말 것.

---

## Context

v0.1 (ADR-001~006) 는 synchronous `Chat(ctx, req) → (ChatResponse, error)` 만 지원. 모든 LLM gateway 의 #1 요청 기능은 **streaming** — 토큰이 하나씩 도착할 때 caller 의 UI 가 typewriter 효과를 줄 수 있어야 함. LiteLLM / Portkey / Helicone 모두 streaming 이 baseline.

영향 모듈:

- `pkg/provider` (수정) — Provider 인터페이스 확장 vs 별 인터페이스 (Q2). 기존 `Chat` 시그니처는 비파괴 보존.
- `pkg/provider/openai`, `pkg/provider/anthropic`, `pkg/provider/gemini` — 각 vendor SSE 포맷 파싱.
- `gateway.Chat` (또는 신규 `gateway.ChatStream`) — Streaming 의 failover 의미 (Q4) + ctx cancellation (Q6) + metrics (Q5) 통합.
- `pkg/metrics` (확장) — TTFT (first-token-latency) metric 추가, ADR-006 의 attempt_duration_seconds 와의 관계 정리.

추가 제약:

- ADR-002 의 "vendor-neutral wire" 원칙 — vendor-specific stream event vocabulary 를 caller 에게 누설하지 않음. delta 단위로 정규화.
- ADR-004 의 failover 정책 — partial output 후의 failover 가능 여부 재검토 (Q4).
- ADR-006 의 metric label 카디널리티 — 새 metric 추가 시 같은 cardinality 규칙.
- v0.1 Provider 인터페이스의 backward compatibility — caller 가 `Chat` 만 호출하는 코드는 변경 없이 동작.

---

## Decision

### Q1. Stream return type

- **Decision:** **A — `<-chan StreamChunk` (receive-only channel) + `error` for the pre-stream init failure path**. 별도 `Close()` 메서드 노출 X — caller 가 ctx cancel 로 종료.
- **Agent reasoning:**
  - **A. Receive-only channel** ✅ — Go-idiomatic. `for chunk := range stream { ... }` 자연. ctx cancel 시 producer 가 channel close. select 로 다른 이벤트와 fan-in 자유.
  - **B. Range-over-func iterator (Go 1.23+)** — `func(yield func(StreamChunk) bool)`. 깔끔하지만 error 채널이 yield 시그니처에 못 들어감 → side-channel 필요. 또 producer 가 yield 호출 사이에 ctx 체크해야 — channel 보다 cancellation lifecycle 모호.
  - **C. Callback `func(StreamChunk) error`** — caller 가 chunk 단위 처리 강제. async UI (예: 별 goroutine 에서 chunk 받아 ws 로 push) 와 안 맞음.
  - **D. io.Reader** — 너무 low-level. caller 가 SSE 파싱 다시 함. v0.1 의 "vendor-neutral" 원칙 위반.
- **왜 이 결정이 정당한가:** Channel 은 Go stdlib 의 streaming primitive. select / fan-in / ctx 와의 통합이 free. 라이브러리 사용자가 추가 학습 비용 0.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q2. Provider 인터페이스 통합

- **Decision:** **B — 별 `StreamingProvider` 인터페이스**. 기존 `Provider` 와 분리, adapter 가 선택적 구현.
  ```go
  // 기존 (v0.1, 비파괴):
  type Provider interface {
      Chat(ctx, ChatRequest) (ChatResponse, error)
      Name() string
      SupportsModel(string) bool
      KeyHash() string
  }

  // 신규 (v0.2):
  type StreamingProvider interface {
      Provider // embed
      ChatStream(ctx, ChatRequest) (<-chan StreamChunk, error)
  }
  ```
- **Agent reasoning:**
  - **A. `Provider` 에 `ChatStream` 추가** — BREAKING. 모든 외부 adapter (커뮤니티 작성 OTel / Bedrock 등) 가 시그니처 변경 강제. v0.2 가 v0.1 사용자에게 마이그레이션 부담.
  - **B. 별 `StreamingProvider`** ✅ — type assertion 패턴. `if sp, ok := p.(StreamingProvider); ok { ... }`. 모든 vendor 가 streaming 지원하면 의미 없는 분리 같지만, (1) v0.1 사용자 비파괴 보장, (2) 미래에 streaming 미지원 vendor (예: 로컬 모델 wrapper) 가 등장해도 인터페이스 모순 없음.
  - **C. `Capabilities()` flag + 단일 인터페이스** — runtime flag 로 분기. type 안전성 손실. Go 의 interface satisfaction 이 capability 표현의 자연스러운 도구.
- **왜 이 결정이 정당한가:** Go stdlib 의 패턴 (`io.Writer` vs `io.StringWriter`, `http.Hijacker` 등). v0.1 의 Provider 인터페이스를 "stable" 로 약속한 README 와 일치.
- **Sub-decision (`gateway.ChatStream` 메서드):** Gateway 도 `ChatStream` 메서드를 신설. 내부에서 candidates 중 `StreamingProvider` 만 필터링 (`for _, p := range candidates { if sp, ok := p.(StreamingProvider); ok { ... } }`). caller 가 routing+streaming 한 줄에 가능.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q3. StreamChunk shape

- **Decision:** **delta-based + 마지막 chunk 에 final metadata (Usage / FinishReason)**.
  ```go
  type StreamChunk struct {
      // ContentDelta is the incremental text token(s) for this chunk.
      // Empty on metadata-only chunks (first chunk with role, last chunk
      // with usage). Concatenating ContentDelta across all chunks yields
      // the same string ChatResponse.Content would return.
      ContentDelta string

      // FinishReason is non-empty only on the terminal chunk. Use this
      // (not channel close) to detect normal end-of-stream — channel close
      // can also mean ctx cancellation.
      FinishReason provider.FinishReason

      // Usage is populated only on the terminal chunk. Gemini and
      // Anthropic emit it inline; OpenAI requires stream_options.
      // include_usage=true (the adapter sets it automatically).
      Usage provider.Usage

      // Raw is the original vendor event JSON for advanced callers
      // (tool_use delta, multi-modal blocks). Mirrors ChatResponse.Raw.
      Raw json.RawMessage
  }
  ```
- **Agent reasoning:** Three options compared:
  - **delta-based** ✅ — OpenAI 의 `choices[].delta.content` 와 직접 매핑. Anthropic 의 `content_block_delta` event 도 자연 매핑. 메모리 효율 (cumulative 는 매 chunk 마다 전체 누적 본문 복사). Gemini 의 streamGenerateContent 도 delta 단위.
  - **cumulative** — 매 chunk 가 전체 누적 본문. caller 가 diff 안 해도 됨. 하지만 메모리 O(n²) (n=chunk 수). 50-token 응답 = 50번의 누적 복사.
  - **vendor-raw passthrough** — caller 가 vendor SDK 와 동일한 raw event 받음. vendor-neutral 원칙 위반.
- **왜 이 결정이 정당한가:** OpenAI/Anthropic/Gemini 모두 wire 가 delta-based. cumulative 로 정규화하면 adapter 가 매 chunk 마다 누적 — gateway 가 vendor 비용 외에 별 비용 발생. caller 가 cumulative 가 필요하면 한 줄 `strings.Builder` accumulator.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q4. Streaming + Failover 의 의미

- **Decision:** **first chunk 받기 전까지만 failover. 첫 chunk 이후 retriable 에러는 stream 종료 (failover 안 함)**.
  - 즉 "pre-stream failover" 만 인정. mid-stream failure 는 caller 가 에러 받고 자체 처리 (재시도 = 새 ChatStream 호출).
- **Agent reasoning:**
  - **mid-stream failover 의 문제**: vendor A 가 10 token 보내고 fail → vendor B 로 fallback 하면 B 는 같은 prompt 로 다시 생성 → caller 의 UI 에 "Hello, this is" 까지 보여졌는데 갑자기 "Greetings from the model" 로 점프. UX 완전히 깨짐.
  - **partial-output rollback**: caller 가 받은 chunk 를 "취소" 하는 메커니즘이 protocol 에 없음. 일부 vendor 는 cumulative=true 옵션 있지만 표준 X.
  - **결정 근거**: streaming 의 본질은 "early commitment to a token stream". 그 commitment 가 깨지면 caller 에게 노출하는 게 최선. mid-stream error chunk 는 Q7 에서 정의.
- **왜 이 결정이 정당한가:** UX 가 protocol 보다 우선. mid-stream failover 가 가능해 보여도 caller 가 받는 텍스트가 inconsistent 면 무의미. ADR-004 의 "no in-provider retry" 원칙과 일관 — early commitment 의 단방향성.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q5. Streaming 메트릭

- **Decision:** **두 신규 metric + 기존 metric 의미 보존**:
  - `llm_gateway_first_token_latency_seconds{vendor, model}` (histogram) — `ChatStream` 호출부터 첫 `ContentDelta!=""` chunk 까지. 운영자의 "perceived responsiveness" 지표.
  - `llm_gateway_stream_duration_seconds{vendor, model, outcome}` (histogram) — `ChatStream` 호출부터 stream close 까지 전체. 첫 chunk 못 받고 종료된 경우 (pre-stream 실패) 는 Origin=vendor 의 `attempt_duration_seconds` 와 동일 의미.
- **Agent reasoning:**
  - **TTFT 중요성**: user-facing UX 의 single most important LLM 메트릭. p50 < 500ms 가 product standard. 없으면 vendor latency 회귀를 dashboard 에서 못 잡음.
  - **stream_duration vs attempt_duration**: 둘 다 histogram, label 다름. ADR-006 의 `attempt_duration_seconds` 는 sync Chat 의 단일 RTT — streaming 의 의미와 다름. 분리 metric 으로 의미 보존.
  - **Cardinality**: TTFT histogram 라벨 = `vendor + model + outcome`. ADR-006 Q7 의 cap (model whitelist) 그대로 적용. 추가 cardinality 비용 없음.
- **Sub-decision (sync Chat 의 attempt_duration_seconds 와 통합 옵션):** reject. streaming 과 sync 의 의미 단위가 다름 (sync = 1 RTT, stream = stream lifecycle). 같은 metric 에 두 단위가 섞이면 p99 해석 모호.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q6. Ctx cancellation mid-stream

- **Decision:** **ctx 가 Done 되면 producer goroutine 이 즉시 channel close. caller 의 `for chunk := range stream` 자연 종료**. mid-stream error chunk 발행 안 함 (ctx cancel 은 caller 가 이미 알고 있음).
- **Agent reasoning:** Channel close 가 Go 의 표준 EOS (end-of-stream) signal. caller 는 `<-ctx.Done()` 를 별도 select 로 감지하거나, channel close 가 normal vs cancellation 인지 ctx.Err() 로 구분. 추가 메서드 도입 (`Cancel()`, `Stop()`) 은 ctx 의 idiom 과 중복.
- **Sub-decision (in-flight HTTP request 처리):** ctx cancel → underlying `*http.Request` 도 ctx-bound → transport 가 connection close. SSE stream 의 partial bytes 는 discard. goroutine leak 없음.
- **왜 이 결정이 정당한가:** Go 의 ctx + channel 조합은 stdlib 의 streaming convention. 라이브러리가 자체 cancellation API 도입하면 학습 비용 + 이중 cancel 경로 (ctx vs `Cancel()`) 의 race.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

### Q7. Mid-stream error propagation

- **Decision:** **error chunk 패턴 — `StreamChunk.Err error` 필드 추가. error 인 chunk 가 보이면 caller 는 break + 처리. 그 후 channel close.**
  ```go
  type StreamChunk struct {
      ContentDelta string
      FinishReason provider.FinishReason
      Usage        provider.Usage
      Raw          json.RawMessage

      // Err is non-nil ONLY on the terminal chunk when the stream failed
      // mid-flight (vendor disconnected, mid-stream 5xx event, JSON parse
      // error on a chunk). When Err is non-nil, ContentDelta is empty and
      // FinishReason is FinishUnknown. The channel closes immediately
      // after this chunk; callers MUST check Err inside the range loop.
      Err error
  }
  ```
- **Agent reasoning:**
  - **separate error channel** rejected — caller 가 두 채널 select 강제. range 루프와 안 어울림. mid-stream error 가 흔하지 않은 경로인데 매 chunk 마다 두 채널 monitoring 부담.
  - **single error chunk** ✅ — Q3 의 StreamChunk 에 한 필드 추가. caller 의 `for chunk := range stream { if chunk.Err != nil { ... } }` 패턴 자연. Q4 의 "mid-stream failover 없음" 결정과 일관 — Err 는 항상 stream 종료 signal.
  - **panic / log + close** rejected — caller 가 silent partial output 받음. observability 측면에서 최악.
- **Sub-decision (Err 타입):** `*provider.ProviderError` 와 일관. caller 가 `errors.As(chunk.Err, &pe)` + Sentinel 패턴 그대로 사용 가능. ADR-002 의 sentinel error 패턴 재사용.
- **왜 이 결정이 정당한가:** Channel close 만으로는 "정상 종료" 와 "에러 종료" 구분 불가 (Q6 의 ctx cancel 도 channel close). FinishReason 만 보면 mid-stream 에러는 Unknown 으로 모호. 명시적 Err 필드가 단일 종료 signal 채널 (channel close) 과 별개의 에러 정보 채널.
- **Maintainer note:** <!-- TODO: 본인 한 줄 voice 로 -->

---

### Synthesis — Go pseudocode

의존성 방향 (ADR-006 와 일관):

```
pkg/types  (leaf)
    ↑
pkg/provider  (Provider + StreamingProvider 인터페이스, StreamChunk)
    ↑
pkg/metrics  (TTFT histogram 추가)
    ↑
pkg/gateway  (Gateway.ChatStream)
```

**Provider 인터페이스 확장:**

```go
// pkg/provider/streaming.go (신규)

type StreamingProvider interface {
    Provider
    ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error)
}

type StreamChunk struct {
    ContentDelta string
    FinishReason FinishReason
    Usage        Usage
    Raw          json.RawMessage
    Err          error
}
```

**Adapter 구현 패턴 (openai/streaming.go 예시):**

```go
func (c *Client) ChatStream(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamChunk, error) {
    // 1. Build SSE HTTP request — same body as Chat() + stream: true.
    // 2. Send request. Pre-stream error (auth, model, build) returns
    //    (nil, *ProviderError) without channel allocation.
    httpResp, err := c.http.Do(httpReq)
    if err != nil { return nil, mapTransportError(ctx, err) }
    if httpResp.StatusCode >= 400 {
        defer httpResp.Body.Close()
        body, _ := io.ReadAll(httpResp.Body)
        return nil, mapHTTPError(httpResp, body)
    }

    // 3. Spawn producer goroutine. Buffered channel = small (16) — SSE
    //    backpressure is OK; vendor sends faster than UI consumes is rare.
    out := make(chan provider.StreamChunk, 16)
    go func() {
        defer close(out)
        defer httpResp.Body.Close()
        scanner := bufio.NewScanner(httpResp.Body)
        for scanner.Scan() {
            // ctx check happens via http.Request ctx-bound — scanner Read
            // returns error on ctx cancel.
            select {
            case <-ctx.Done():
                return
            default:
            }
            chunk, done := parseSSELine(scanner.Bytes())
            if chunk == nil { continue } // keep-alive / comment line
            out <- *chunk
            if done { return }
        }
        if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
            out <- provider.StreamChunk{Err: mapTransportError(ctx, err)}
        }
    }()
    return out, nil
}
```

**Gateway.ChatStream 통합:**

```go
func (g *Gateway) ChatStream(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamChunk, error) {
    primary, fallbacks, rerr := router.PickWithFallbacks(g.providers, req.Model)
    if rerr != nil { return nil, rerr } // router 실패 = pre-stream failure

    candidates := append([]provider.Provider{primary}, fallbacks...)

    // Q4: pre-stream failover only. 첫 candidate 부터 시도, retriable 이면 다음.
    var lastErr error
    for i, p := range candidates {
        sp, ok := p.(provider.StreamingProvider)
        if !ok {
            // Provider 인터페이스 만 구현. streaming 미지원 → skip (router 가
            // 이걸 사전에 filter 하는 게 더 깔끔하지만 v0.2.0 시점에 결정).
            continue
        }
        // Rate-limit pre-check + ctx check (Chat 과 동일)
        stream, err := sp.ChatStream(ctx, req)
        if err == nil {
            // 첫 chunk 받기 전. Metrics: TTFT histogram 은 첫 ContentDelta!="" 시점에 Observe.
            // 이건 wrapper goroutine 으로 intercept.
            return wrapWithMetrics(g, p, i, stream), nil
        }
        lastErr = err
        if !shouldFailover(err) { return nil, err }
        // recordFailover (ADR-006) — pre-stream 이므로 동일 패턴.
    }
    return nil, lastErr
}
```

---

## Consequences

### Positive

- LLM gateway 의 #1 missing 기능 해결. LiteLLM 과의 feature gap 좁힘.
- TTFT metric 으로 운영자의 user-facing UX 가시성 확보.
- Q4 의 "pre-stream failover only" 결정이 streaming UX 의 일관성 보장 — caller 가 받는 stream 은 항상 single-vendor 의 single-attempt.
- Provider 인터페이스 분리 (Q2) 가 v0.1 사용자 비파괴.

### Negative

- mid-stream 에러는 caller 가 자체 처리 (재시도 = 새 ChatStream). gateway-level retry / failover 가 stream 에 없음. 운영자가 "왜 streaming 만 failover 가 약해?" 라는 질문 받음 — Q4 의 UX 정당성 doc 으로 답.
- SSE 파싱 코드 3 vendor 분 추가 (~150 LOC 씩). adapter 의 maintenance surface 증가.
- Streaming 비활성 vendor 가 향후 등장하면 `gateway.ChatStream` 의 candidates 가 빈 슬라이스 = "no streaming provider supports this model" 에러 새 케이스.

### Risks

| Risk | Mitigation |
|---|---|
| Caller 가 channel 을 끝까지 안 읽고 goroutine 만 종료 (gw goroutine leak) | producer goroutine 이 ctx.Done() 감지 → return. caller 는 ctx cancel 책임. 미문서화 시 leak. |
| Mid-stream 5xx event 가 vendor 에 따라 다른 wire 표현 | adapter 별 mapSSEError 로 격리. Q7 의 `Err` 필드가 단일 caller 인터페이스. |
| TTFT 측정의 wrapper 추가가 overhead | wrapper goroutine 1개 추가, channel forward — 1-2 ns 수준. 벤치마크로 검증 후 README 인용. |
| Gemini 의 `streamGenerateContent` 가 SSE 아닌 JSON-line | adapter 가 line-based parsing 추가 (SSE 와 약간 다른 분기). 같은 StreamChunk 로 정규화. |
| `Capabilities()` 가 없어 caller 가 streaming 지원 vendor 알 수 없음 | type assertion 패턴 명시. 향후 `gateway.HasStreamingFor(model)` helper 도입 검토 (v0.3). |

---

## Alternatives Considered

### Alt 1 — Q1 Iterator (range-over-func)

Go 1.23+ 의 `iter.Seq[T]` / `func(yield func(T) bool)`. 1-line `for chunk := range stream { ... }` 깔끔.

**Reject 이유:** error propagation 이 yield 시그니처에 못 들어감 → side-channel (`*error` pointer, error-returning closure 등) 필요. Q7 의 Err 필드 패턴이 iterator 보다 직접적. 또 iterator 는 producer 가 매 yield 사이에 ctx check 책임 — channel 의 select 보다 cancellation 자연도 낮음.

### Alt 2 — Q2 단일 Provider 인터페이스에 `ChatStream` 추가

BREAKING. v0.1 사용자가 외부 adapter 작성했으면 시그니처 추가 강제.

**Reject 이유:** README 가 v0.1 의 Provider 인터페이스를 "stable" 로 약속. v0.2 가 v1.0 가기 전에 깨면 trust loss. StreamingProvider 의 type assertion 패턴이 Go stdlib (`io.StringWriter`, `http.Hijacker`) 의 표준.

### Alt 3 — Q4 Mid-stream failover (cumulative resend)

vendor A 가 fail 하면 받은 partial 을 buffer 에 저장, vendor B 로 새 요청 보내고 cumulative=true 로 받아 buffer 의 prefix 만큼 skip 후 caller 에게 forward. 이론적으로 partial-output preservation 가능.

**Reject 이유:** (1) 모든 vendor 가 cumulative=true 옵션 보장 X (OpenAI 는 delta 만), (2) prefix skip 로직이 token boundary 와 character boundary 의 mismatch (multi-byte UTF-8) 처리 복잡, (3) vendor 가 fail 한 시점의 model state 와 retry 시점의 state 가 다를 수 있어 token 단위 일치 깨질 가능성. 이론적 가능성과 실제 UX 보장 차이가 큼.

### Alt 4 — Q3 Cumulative chunks

매 chunk 가 전체 누적 본문.

**Reject 이유:** O(n²) memory + adapter 에서 매 chunk 마다 누적 복사. caller 가 cumulative 필요하면 `strings.Builder` 한 줄로 가능 — 라이브러리 default 가 더 비싼 옵션을 강제할 이유 없음.

### Alt 5 — Q7 Separate error channel

`(stream <-chan StreamChunk, errs <-chan error, err error)` 트리플 리턴. mid-stream error 가 별 채널.

**Reject 이유:** caller 가 두 채널 select 강제. range 루프 패턴이 깨짐. mid-stream error 는 stream 종료 신호 — 단일 채널의 마지막 chunk Err 필드가 더 명확.

---

## Related ADRs

- [ADR-002](0002-provider-interface-design.md) — Provider 인터페이스 + sentinel error. StreamingProvider 가 이 위에 적층.
- [ADR-004](0004-failover-trigger-and-retry.md) — Q4 의 "pre-stream failover only" 결정이 streaming context 에서 ADR-004 의 단방향 commitment 원칙 재적용.
- [ADR-006](0006-observability.md) — Q5 의 신규 metric (TTFT, stream_duration) 이 ADR-006 의 4-metric 셋에 적층. 같은 cardinality 규칙 (Q7 model whitelist) 적용.

## Open Questions

- [ ] Tool calling 의 delta 정규화 — ADR-007 v1 은 ContentDelta (text) 만. tool_use delta 는 Raw 에 통과. v0.2.x 또는 ADR-009 에서 typed tool calling 결정 시 같이.
- [ ] Multi-modal (image / audio) chunk 정규화 — text 만 다룸. v0.2.x.
- [ ] Streaming-aware rate limit — TPM 측정이 사후 (전체 stream 끝나야 OutputTokens 확정). RPM 은 entry-time, TPM 은 stream-end-time reconcile. ADR-005 의 RPM/TPM 분리 결정이 streaming 에 그대로 적용되는지 v0.2 구현 시점 검증.
- [ ] `gateway.HasStreamingFor(model)` helper — caller 가 streaming 지원 vendor 사전 확인. v0.3 후보.
