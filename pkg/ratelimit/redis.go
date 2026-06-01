package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// RedisBackend is the multi-instance RateLimiter. State lives in two Redis
// sorted sets per bucket (one for RPM, one for TPM) keyed by the
// per-provider × per-key composite (ADR-005 Q5). All read/check/write logic
// runs inside a single Lua script so concurrent gateway instances observe a
// strictly serialized view of the window — the same atomicity guarantee
// MemoryBackend gets from sync.Mutex, extended across processes.
//
// Record stays a no-op for parity with MemoryBackend: the conservative
// reservation made in Allow ages out of the window naturally. A future ADR
// can introduce refund-on-Record once Usage telemetry justifies the extra
// round trip.
type RedisBackend struct {
	client redis.UniversalClient
	opts   Options
	script *redis.Script
}

// allowScript runs ZREMRANGEBYSCORE pruning, RPM/TPM totals, and the
// reservation ZADDs inside one EVAL/EVALSHA. The TPM ZSET stores
// "<tokens>:<nonce>" as the member with score=timestamp; this preserves
// pruning by time while letting the script sum reserved tokens by parsing
// the member prefix. The caller-supplied nonce (ARGV[6]) is what keeps
// ZSET members unique under same-second / same-token bursts — without it
// concurrent reservations would deduplicate and silently under-count.
//
// Return shape: {1} on allow, {0, "rpm"|"tpm", retry_after_seconds} on deny.
// retry_after is the seconds until the oldest in-window entry on the
// blocking dimension expires (best-effort hint, callers may use it on the
// *ProviderError they surface).
const allowScript = `
local now    = tonumber(ARGV[1])
local window = tonumber(ARGV[5])
local cutoff = now - window
local tokens = tonumber(ARGV[4])
local nonce  = ARGV[6]
local rpmLim = tonumber(ARGV[2])
local tpmLim = tonumber(ARGV[3])

redis.call('ZREMRANGEBYSCORE', KEYS[1], 0, cutoff)
redis.call('ZREMRANGEBYSCORE', KEYS[2], 0, cutoff)

if rpmLim > 0 then
  local rpmUsed = redis.call('ZCARD', KEYS[1])
  if rpmUsed + 1 > rpmLim then
    local oldest = redis.call('ZRANGE', KEYS[1], 0, 0, 'WITHSCORES')
    local retry = window
    if oldest[2] then retry = (tonumber(oldest[2]) + window) - now end
    if retry < 0 then retry = 0 end
    return {0, 'rpm', retry}
  end
end

if tpmLim > 0 then
  local entries = redis.call('ZRANGEBYSCORE', KEYS[2], cutoff, '+inf')
  local tpmUsed = 0
  for i = 1, #entries do
    local member = entries[i]
    local sep = string.find(member, ':')
    if sep then tpmUsed = tpmUsed + tonumber(string.sub(member, 1, sep - 1)) end
  end
  if tpmUsed + tokens > tpmLim then
    local oldest = redis.call('ZRANGE', KEYS[2], 0, 0, 'WITHSCORES')
    local retry = window
    if oldest[2] then retry = (tonumber(oldest[2]) + window) - now end
    if retry < 0 then retry = 0 end
    return {0, 'tpm', retry}
  end
end

redis.call('ZADD', KEYS[1], now, tostring(now) .. ':' .. nonce)
redis.call('ZADD', KEYS[2], now, tostring(tokens) .. ':' .. nonce)
redis.call('EXPIRE', KEYS[1], window)
redis.call('EXPIRE', KEYS[2], window)
return {1}
`

// NewRedis constructs a Redis-backed RateLimiter. The client is borrowed,
// not owned — callers are responsible for its lifecycle (Close, pool
// settings, sentinel/cluster topology).
func NewRedis(client redis.UniversalClient, opts Options) *RedisBackend {
	return &RedisBackend{
		client: client,
		opts:   opts,
		script: redis.NewScript(allowScript),
	}
}

const windowSeconds = 60

// Allow runs the atomic Lua check. The window is a fixed 60s sliding log
// to match vendor RPM/TPM measurement semantics (ADR-005 Q2).
//
// Both limits zero short-circuits without touching Redis — a no-config
// backend would otherwise pay the round-trip just to learn it has no work
// to do, which makes "limiter wired but unconfigured" unusably expensive.
//
// Backend errors are surfaced verbatim; the caller (gateway.Chat) is
// responsible for the FailOpen/FailClosed decision per ADR-005 Q6.
func (r *RedisBackend) Allow(ctx context.Context, providerName, apiKeyHash string, req provider.ChatRequest) (Decision, error) {
	if r.opts.Limits.RequestsPerMinute == 0 && r.opts.Limits.TokensPerMinute == 0 {
		return Decision{Allow: true}, nil
	}

	nonce, err := newNonce()
	if err != nil {
		return Decision{}, fmt.Errorf("ratelimit/redis: nonce: %w", err)
	}

	now := r.opts.now().Unix()
	tokens := EstimateTokens(req)
	base := bucketKey(providerName, apiKeyHash)
	rpmKey := "ratelimit:" + base + ":rpm"
	tpmKey := "ratelimit:" + base + ":tpm"

	res, err := r.script.Run(ctx, r.client,
		[]string{rpmKey, tpmKey},
		now,
		r.opts.Limits.RequestsPerMinute,
		r.opts.Limits.TokensPerMinute,
		tokens,
		windowSeconds,
		nonce,
	).Result()
	if err != nil {
		return Decision{}, fmt.Errorf("ratelimit/redis: script: %w", err)
	}

	return parseDecision(res)
}

// Record is intentionally a no-op for v0.1 parity with MemoryBackend — the
// over-reservation made in Allow ages out of the window naturally. Refund
// logic against a separate ZSET entry can land in a follow-up once Usage
// data shows the over-conservatism is material.
func (r *RedisBackend) Record(_ context.Context, _, _ string, _ provider.Usage) error {
	return nil
}

func parseDecision(raw any) (Decision, error) {
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return Decision{}, fmt.Errorf("ratelimit/redis: unexpected script return: %T", raw)
	}

	verdict, ok := arr[0].(int64)
	if !ok {
		return Decision{}, fmt.Errorf("ratelimit/redis: verdict not int64: %T", arr[0])
	}

	if verdict == 1 {
		return Decision{Allow: true}, nil
	}

	// Denied. arr is {0, dimension, retry_after_seconds}; we only surface
	// RetryAfter — dimension is informational and stays in the ADR's
	// future metric label space (ADR-006 input).
	if len(arr) < 3 {
		return Decision{Allow: false}, nil
	}
	secs, ok := arr[2].(int64)
	if !ok {
		// Lua sometimes returns floats through redigo/go-redis; tolerate.
		if s, sok := arr[2].(string); sok {
			if v, err := strconv.ParseInt(s, 10, 64); err == nil {
				secs = v
				ok = true
			}
		}
	}
	if !ok {
		return Decision{Allow: false}, nil
	}
	if secs < 0 {
		secs = 0
	}
	d := time.Duration(secs) * time.Second
	return Decision{Allow: false, RetryAfter: &d}, nil
}

// newNonce returns 16 hex chars sourced from crypto/rand. Short enough to
// keep ZSET members compact, long enough to make collision across the
// 60-second window negligible (2^32 space vs at most a few thousand
// concurrent entries per bucket in practice).
func newNonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("rand read failed")
	}
	return hex.EncodeToString(b[:]), nil
}

// Compile-time interface satisfaction.
var _ RateLimiter = (*RedisBackend)(nil)
