// Package anthropic implements provider.Provider against Anthropic's
// Messages API. See docs/adr/0002-provider-interface-design.md for the
// mapping rules between Anthropic's wire format and the gateway-neutral
// types — in particular: system as a top-level field, max_tokens as a
// required parameter, content as an array of typed blocks, and the
// vendor-specific overloaded_error category.
package anthropic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

const (
	vendorName        = "anthropic"
	defaultBaseURL    = "https://api.anthropic.com/v1"
	defaultAPIVersion = "2023-06-01"
	defaultTimeout    = 60 * time.Second
)

// defaultModels enumerates the Anthropic Messages-API models the adapter
// recognizes at build time. Add via WithModels for early access or a
// follow-up PR for permanent inclusion.
var defaultModels = map[string]struct{}{
	"claude-haiku-4-5-20251001": {},
	"claude-opus-4-7":           {},
	"claude-sonnet-4-6":         {},
}

// Client is the Anthropic adapter.
type Client struct {
	apiKey           string
	baseURL          string
	apiVersion       string
	http             *http.Client
	models           map[string]struct{}
	defaultMaxTokens int // 0 means "unset" — Chat() returns InvalidInput on nil MaxTokens
}

// Option configures Client at construction time.
type Option func(*Client)

// WithBaseURL points the client at a non-default endpoint (proxy, httptest
// server). Default is the public Anthropic API.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = u }
}

// WithHTTPClient injects a custom *http.Client. Nil is ignored.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h == nil {
			return
		}
		c.http = h
	}
}

// WithModels replaces the supported-model set.
func WithModels(models []string) Option {
	return func(c *Client) {
		c.models = make(map[string]struct{}, len(models))
		for _, m := range models {
			c.models[m] = struct{}{}
		}
	}
}

// WithDefaultMaxTokens sets the fallback used when a ChatRequest arrives
// with MaxTokens == nil. Anthropic requires max_tokens; without a fallback
// the adapter returns ErrorTypeInvalidInput for nil. See ADR-002 Q2.
func WithDefaultMaxTokens(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.defaultMaxTokens = n
		}
	}
}

// WithAPIVersion overrides the `anthropic-version` header. Default is the
// stable revision the adapter was developed against.
func WithAPIVersion(v string) Option {
	return func(c *Client) {
		if v != "" {
			c.apiVersion = v
		}
	}
}

// New constructs an Anthropic provider client. apiKey is required.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:     apiKey,
		baseURL:    defaultBaseURL,
		apiVersion: defaultAPIVersion,
		http:       &http.Client{Timeout: defaultTimeout},
		models:     defaultModels,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Name returns "anthropic".
func (c *Client) Name() string { return vendorName }

// SupportsModel reports whether this client can serve the given model id.
func (c *Client) SupportsModel(model string) bool {
	_, ok := c.models[model]
	return ok
}

// KeyHash returns a stable hex SHA-256 of the configured API key. The
// plaintext key never leaves this struct.
func (c *Client) KeyHash() string {
	sum := sha256.Sum256([]byte(c.apiKey))
	return hex.EncodeToString(sum[:])
}

// Wire types. Package-private — callers go through provider.ChatRequest /
// provider.ChatResponse only.

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type wireRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"`
}

type wireContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type wireResponse struct {
	Content    []wireContentBlock `json:"content"`
	StopReason string             `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type wireError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Chat issues a Messages-API request and maps the response into the
// gateway-neutral provider.ChatResponse / *provider.ProviderError types.
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	if !c.SupportsModel(req.Model) {
		return provider.ChatResponse{}, provider.NewProviderError(
			vendorName, provider.ErrorTypeInvalidInput, 0, false,
			fmt.Sprintf("unsupported model %q", req.Model), nil)
	}

	maxTokens, err := c.resolveMaxTokens(req.MaxTokens)
	if err != nil {
		return provider.ChatResponse{}, err
	}

	msgs := make([]wireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, wireMessage{Role: string(m.Role), Content: m.Content})
	}

	body, err := json.Marshal(wireRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
		System:    req.System,
		Messages:  msgs,
	})
	if err != nil {
		return provider.ChatResponse{}, provider.NewProviderError(
			vendorName, provider.ErrorTypeUnknown, 0, false, "encode request", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return provider.ChatResponse{}, provider.NewProviderError(
			vendorName, provider.ErrorTypeUnknown, 0, false, "build http request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", c.apiVersion)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return provider.ChatResponse{}, mapTransportError(ctx, err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return provider.ChatResponse{}, provider.NewProviderError(
			vendorName, provider.ErrorTypeUnknown, httpResp.StatusCode, false, "read response", err)
	}

	if httpResp.StatusCode >= 400 {
		return provider.ChatResponse{}, mapHTTPError(httpResp, raw)
	}

	var parsed wireResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return provider.ChatResponse{}, provider.NewProviderError(
			vendorName, provider.ErrorTypeUnknown, httpResp.StatusCode, false, "decode response", err)
	}

	return provider.ChatResponse{
		Content:      joinTextBlocks(parsed.Content),
		FinishReason: mapStopReason(parsed.StopReason),
		Usage: provider.Usage{
			InputTokens:  parsed.Usage.InputTokens,
			OutputTokens: parsed.Usage.OutputTokens,
		},
		Raw: raw,
	}, nil
}

// resolveMaxTokens implements ADR-002 Q2: nil falls back to the
// adapter-configured default; if neither is set, the adapter returns
// InvalidInput BEFORE hitting the wire (Anthropic would 400 anyway).
func (c *Client) resolveMaxTokens(reqMax *int) (int, error) {
	switch {
	case reqMax != nil:
		if *reqMax <= 0 {
			return 0, provider.NewProviderError(vendorName, provider.ErrorTypeInvalidInput, 0, false,
				"MaxTokens must be > 0", nil)
		}
		return *reqMax, nil
	case c.defaultMaxTokens > 0:
		return c.defaultMaxTokens, nil
	default:
		return 0, provider.NewProviderError(vendorName, provider.ErrorTypeInvalidInput, 0, false,
			"MaxTokens is required by Anthropic; set ChatRequest.MaxTokens or anthropic.WithDefaultMaxTokens", nil)
	}
}

// joinTextBlocks extracts text blocks from the content array and joins
// them with "\n" per the gateway contract (ADR-002 ChatResponse.Content).
// Non-text blocks (tool_use, thinking, image, ...) are intentionally
// dropped from Content — callers reach for ChatResponse.Raw.
func joinTextBlocks(blocks []wireContentBlock) string {
	var b strings.Builder
	first := true
	for _, block := range blocks {
		if block.Type != "text" {
			continue
		}
		if !first {
			b.WriteByte('\n')
		}
		b.WriteString(block.Text)
		first = false
	}
	return b.String()
}

func mapStopReason(r string) provider.FinishReason {
	switch r {
	case "end_turn":
		return provider.FinishStop
	case "max_tokens":
		return provider.FinishLength
	case "stop_sequence":
		return provider.FinishStopSequence
	case "tool_use":
		return provider.FinishToolUse
	default:
		return provider.FinishUnknown
	}
}

func mapTransportError(ctx context.Context, err error) *provider.ProviderError {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return provider.NewProviderError(vendorName, provider.ErrorTypeTimeout, 0, true, "request deadline exceeded", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return provider.NewProviderError(vendorName, provider.ErrorTypeTimeout, 0, false, "request canceled", err)
	}
	return provider.NewProviderError(vendorName, provider.ErrorTypeServer, 0, true, "transport error", err)
}

func mapHTTPError(resp *http.Response, body []byte) *provider.ProviderError {
	var wire wireError
	_ = json.Unmarshal(body, &wire) // tolerate non-JSON
	msg := wire.Error.Message
	if msg == "" {
		msg = "no error message"
	}
	errType := wire.Error.Type

	var (
		typ       provider.ErrorType
		retriable bool
	)
	switch {
	case errType == "overloaded_error":
		// Anthropic-specific signal; usually arrives with 503 but the type
		// is the authoritative marker. Router treats this as a separate
		// failover trigger from generic 5xx.
		typ, retriable = provider.ErrorTypeOverloaded, true
	case resp.StatusCode == 429:
		typ, retriable = provider.ErrorTypeRateLimit, true
	case resp.StatusCode == 401:
		typ, retriable = provider.ErrorTypeAuth, false
	case resp.StatusCode == 403:
		typ, retriable = provider.ErrorTypePermission, false
	case resp.StatusCode == 404:
		typ, retriable = provider.ErrorTypeNotFound, false
	case resp.StatusCode == 400:
		typ, retriable = provider.ErrorTypeInvalidInput, false
	case resp.StatusCode >= 500:
		typ, retriable = provider.ErrorTypeServer, true
	default:
		typ, retriable = provider.ErrorTypeUnknown, false
	}

	pe := provider.NewProviderError(vendorName, typ, resp.StatusCode, retriable, msg, nil)
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if d, ok := parseRetryAfter(ra); ok {
			pe = pe.WithRetryAfter(d)
		}
	}
	return pe
}

// parseRetryAfter handles both delta-seconds and HTTP-date per RFC 7231.
func parseRetryAfter(s string) (time.Duration, bool) {
	if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(s); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// Compile-time interface satisfaction. Placed in production file so
// `go build` catches signature drift.
var _ provider.Provider = (*Client)(nil)
