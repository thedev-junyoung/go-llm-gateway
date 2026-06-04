// Package provider defines the contract every LLM vendor adapter
// (OpenAI, Anthropic, ...) implements. The router, rate limiter, and
// metrics packages depend only on this surface — never on vendor SDKs
// directly. See docs/adr/0002-provider-interface-design.md.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Provider is the chat-completion contract every vendor adapter implements.
type Provider interface {
	// Chat issues a synchronous chat completion. The router calls SupportsModel
	// before Chat; adapters MUST still validate at entry and return
	// *ProviderError with Type == ErrorTypeInvalidInput when an unsupported
	// model leaks through.
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)

	// Name identifies the vendor: "openai", "anthropic", ... Used by metrics
	// and logging; not for application-level branching (see ProviderError.Vendor).
	Name() string

	// SupportsModel reports whether this provider can serve the given vendor
	// model id (e.g. "gpt-4o", "claude-opus-4-7"). Hot-path check — must not
	// make network calls.
	SupportsModel(model string) bool

	// KeyHash returns a stable, opaque identifier for the credential this
	// provider is configured with — concretely, the hex SHA-256 of the API
	// key. The plaintext key never leaves the adapter; only the hash crosses
	// package boundaries. The rate-limit bucket key (ADR-005 Q5) combines
	// Name() and KeyHash() so two clients of the same vendor with different
	// keys count against separate quotas.
	KeyHash() string
}

// Role identifies who authored a message. There is intentionally no
// RoleSystem: the system prompt rides on ChatRequest.System so the
// Anthropic top-level field is representable without lossy translation.
type Role string

// Role values recognized by adapters. There is intentionally no system role.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn in the conversation history.
type Message struct {
	Role    Role
	Content string
}

// ChatRequest is the vendor-neutral input to Chat.
type ChatRequest struct {
	// Model is the vendor model id, passed through verbatim.
	Model string

	// System is the optional system prompt. Adapters route it per-vendor —
	// Anthropic top-level field, OpenAI synthetic role="system" message.
	System string

	// Messages alternate user/assistant. Do NOT include system here.
	Messages []Message

	// MaxTokens caps generation. nil means "use adapter-configured default";
	// the OpenAI adapter passes nil through (vendor default applies) while
	// the Anthropic adapter substitutes its configured fallback or returns
	// ErrorTypeInvalidInput. See ADR-002 Q2 and Consequences/Negative.
	MaxTokens *int
}

// FinishReason is the gateway-normalized vocabulary for why generation stopped.
type FinishReason string

const (
	// FinishStop covers natural completion. OpenAI's "stop" maps here for
	// both natural-end and stop-sequence cases (vendor can't distinguish).
	FinishStop FinishReason = "stop"

	// FinishLength means the response was truncated by max_tokens.
	FinishLength FinishReason = "length"

	// FinishContentFilter is a safety block. OpenAI-only — Anthropic's
	// stop_reason has no direct equivalent; adapters there emit FinishStop
	// and may signal safety in Raw.
	FinishContentFilter FinishReason = "content_filter"

	// FinishStopSequence is Anthropic-only — OpenAI's "stop" covers this too
	// and is mapped to FinishStop. Documented asymmetry, not a bug.
	FinishStopSequence FinishReason = "stop_sequence"

	// FinishToolUse means the model wants to invoke a tool. v0.1 exposes
	// the signal only; the typed tool-calling surface is deferred to v0.2,
	// so callers receiving this value parse ChatResponse.Raw themselves.
	FinishToolUse FinishReason = "tool_use"

	// FinishUnknown is the forward-compat sink: vendor emitted a reason
	// this version doesn't recognize. Adapters MUST emit this rather than
	// panicking or silently coercing.
	FinishUnknown FinishReason = "unknown"
)

// Usage reports token consumption for cost and rate-limit accounting.
// Cache-related fields (Anthropic prompt caching) are deferred to v0.2;
// v0.1 cost metrics assume cache miss as a conservative upper bound.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// ChatResponse is the vendor-neutral output of Chat.
type ChatResponse struct {
	// Content is the joined plain text. Adapters MUST use "\n" (LF) as the
	// join separator when a vendor returns multiple text blocks so the same
	// request yields byte-identical Content on either provider.
	Content string

	// FinishReason is the gateway-normalized stop signal.
	FinishReason FinishReason

	// Usage carries token counts for accounting.
	Usage Usage

	// Raw is the original vendor response body. It is the v0.1 escape hatch
	// for tool_use, multi-modal blocks, Anthropic safety signals, and
	// prompt-caching tokens. Stability follows the vendor schema, not the
	// gateway's semver — callers that parse Raw own the regression risk on
	// vendor SDK upgrades. See ADR-002 Consequences/Negative.
	Raw json.RawMessage

	// Attempts is the per-attempt trace of the Chat call (primary + any
	// fallbacks the router tried). Caller-facing — see ADR-006 Q4. Adapters
	// MUST NOT set this field; the gateway populates it. Empty on error paths
	// that fail before any provider was reached.
	//
	// Same data is fed to MetricRecorder.OnAttempt for the metric/log sink;
	// the two paths target different audiences (UI / request_id correlation
	// vs operator dashboard). See ADR-006 Q4 sub-decision on diverge risk
	// if the caller mutates the slice.
	Attempts []AttemptInfo
}

// ErrorType is the gateway-normalized error category. Adapters map vendor
// error vocabularies onto these constants so the router branches on a
// closed set without inspecting strings.
type ErrorType string

// ErrorType values. Router-critical categories (RateLimit, Overloaded, Auth,
// Timeout) also have matching sentinel errors below; the rest are reached via
// errors.As + Type inspection. See ADR-002 Q5.
const (
	ErrorTypeRateLimit    ErrorType = "rate_limit"
	ErrorTypeOverloaded   ErrorType = "overloaded"
	ErrorTypeAuth         ErrorType = "auth"
	ErrorTypePermission   ErrorType = "permission"
	ErrorTypeInvalidInput ErrorType = "invalid_input"
	ErrorTypeNotFound     ErrorType = "not_found"
	ErrorTypeServer       ErrorType = "server"
	ErrorTypeTimeout      ErrorType = "timeout"
	ErrorTypeUnknown      ErrorType = "unknown"
)

// ProviderError carries enough information for the router to choose retry,
// failover, or abort without parsing vendor-specific strings. Construct via
// NewProviderError — the unexported vendor/message/wrapped fields cannot be
// set from outside this package, which is intentional (see ADR-002 Q5).
//
// Note: external callers can still build a literal that sets only the
// exported fields (Type / Retriable / StatusCode / RetryAfter), leaving
// vendor/message/wrapped at zero values. NewProviderError is the contract;
// the type system catches only the unexported fields.
//
// TODO(#41): if any adapter PR slips a literal-init bypass, move every
// field unexported and expose Type/Retriable/StatusCode/RetryAfter via
// accessor methods only.
//
// The name intentionally repeats the package: provider.Error would collide
// with the built-in `error` interface in reader's heads, while ProviderError
// is unambiguous at every call site (errors.As(err, &pe *provider.ProviderError)).
//
//nolint:revive // stuttering accepted; see name rationale above.
type ProviderError struct {
	Type       ErrorType
	Retriable  bool
	RetryAfter *time.Duration
	StatusCode int

	vendor  string
	message string
	wrapped error
}

// NewProviderError is the only path to set the unexported vendor / message /
// wrapped fields. Adapter packages (pkg/provider/openai, pkg/provider/anthropic)
// MUST use this constructor.
func NewProviderError(vendor string, t ErrorType, statusCode int, retriable bool, msg string, wrapped error) *ProviderError {
	return &ProviderError{
		Type:       t,
		Retriable:  retriable,
		StatusCode: statusCode,
		vendor:     vendor,
		message:    msg,
		wrapped:    wrapped,
	}
}

// WithRetryAfter attaches a vendor-provided backoff hint and returns the
// receiver so adapters can chain. Only set when the vendor response actually
// carries Retry-After (or equivalent).
func (e *ProviderError) WithRetryAfter(d time.Duration) *ProviderError {
	e.RetryAfter = &d
	return e
}

// Vendor returns the vendor identifier. Keeping the field unexported blocks
// the direct-field anti-pattern (`if err.vendor == "openai"`) at compile
// time, but `err.Vendor() == "openai"` still works at runtime — that path
// is discouraged by convention, not by the type system. Call sites that need
// to branch should use errors.Is for sentinels or errors.As + ErrorType.
func (e *ProviderError) Vendor() string { return e.vendor }

// VendorMessage returns the original vendor error message. Debug / log only
// — MUST NOT be returned verbatim to external clients (may contain internal
// state). Sanitize before any HTTP response surface.
func (e *ProviderError) VendorMessage() string { return e.message }

// Error returns a sanitized representation safe to surface in default logs.
// It deliberately omits the vendor's raw message; reach for VendorMessage()
// when you have a sanitization path.
func (e *ProviderError) Error() string {
	return fmt.Sprintf("provider %s: %s (status=%d, retriable=%t)",
		e.vendor, e.Type, e.StatusCode, e.Retriable)
}

// Unwrap exposes the underlying error for errors.Is/errors.As traversal.
func (e *ProviderError) Unwrap() error { return e.wrapped }

// Is supports errors.Is(err, ErrRateLimited) and the other router-critical
// sentinels. Only four categories are matched here — other ErrorType values
// are reached through errors.As + Type inspection (see ADR-002 Q5).
func (e *ProviderError) Is(target error) bool {
	switch target {
	case ErrRateLimited:
		return e.Type == ErrorTypeRateLimit
	case ErrOverloaded:
		return e.Type == ErrorTypeOverloaded
	case ErrAuthFailed:
		return e.Type == ErrorTypeAuth
	case ErrTimeout:
		return e.Type == ErrorTypeTimeout
	}
	return false
}

// Router-critical sentinel errors. These four map directly to a router
// decision branch (retry / failover / abort) and are the only categories
// promoted to sentinels in v0.1.
var (
	ErrRateLimited = errors.New("provider: rate limited")
	ErrOverloaded  = errors.New("provider: overloaded")
	ErrAuthFailed  = errors.New("provider: authentication failed")
	ErrTimeout     = errors.New("provider: timeout")
)

// Compile-time assertion that *ProviderError satisfies the error interface.
// Placed in provider.go (not _test.go) so `go build` — not just `go test` —
// catches accidental removal of the Error() method during maintenance.
var _ error = (*ProviderError)(nil)
