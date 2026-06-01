// Package openai implements provider.Provider against OpenAI's Chat
// Completions API. See docs/adr/0002-provider-interface-design.md for the
// mapping rules between the OpenAI wire format and the gateway-neutral types.
package openai

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
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

const (
	vendorName     = "openai"
	defaultBaseURL = "https://api.openai.com/v1"
	defaultTimeout = 60 * time.Second
)

// defaultModels enumerates the OpenAI Chat Completions models the adapter
// knows at build time. Add new models via WithModels or a follow-up PR; the
// router checks SupportsModel before Chat, so adding a model here unlocks it.
var defaultModels = map[string]struct{}{
	"gpt-3.5-turbo": {},
	"gpt-4":         {},
	"gpt-4-turbo":   {},
	"gpt-4o":        {},
	"gpt-4o-mini":   {},
	"o1-mini":       {},
	"o1-preview":    {},
}

// Client is the OpenAI adapter.
type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
	models  map[string]struct{}
}

// Option configures Client at construction time.
type Option func(*Client)

// WithBaseURL points the client at a non-default endpoint (Azure OpenAI,
// a corporate proxy, a httptest server, etc.).
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = u }
}

// WithHTTPClient injects a custom *http.Client — usually for timeouts,
// instrumentation, or test fakes. Nil is ignored so a misconfigured caller
// doesn't get a nil-deref inside Chat().
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h == nil {
			return
		}
		c.http = h
	}
}

// WithModels replaces the supported-model set. Use to opt new models in
// without waiting for an upstream release.
func WithModels(models []string) Option {
	return func(c *Client) {
		c.models = make(map[string]struct{}, len(models))
		for _, m := range models {
			c.models[m] = struct{}{}
		}
	}
}

// New constructs an OpenAI provider client. apiKey is required.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:  apiKey,
		baseURL: defaultBaseURL,
		http:    &http.Client{Timeout: defaultTimeout},
		models:  defaultModels,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Name returns "openai".
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

// Wire types. Kept package-private — callers go through provider.ChatRequest /
// provider.ChatResponse only.

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type wireRequest struct {
	Model     string        `json:"model"`
	Messages  []wireMessage `json:"messages"`
	MaxTokens *int          `json:"max_tokens,omitempty"`
}

type wireChoice struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

type wireResponse struct {
	Choices []wireChoice `json:"choices"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type wireError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// Chat issues a Chat Completions request and maps the response into the
// gateway-neutral provider.ChatResponse / *provider.ProviderError types.
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	if !c.SupportsModel(req.Model) {
		return provider.ChatResponse{}, provider.NewProviderError(
			vendorName, provider.ErrorTypeInvalidInput, 0, false,
			fmt.Sprintf("unsupported model %q", req.Model), nil)
	}

	msgs := make([]wireMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, wireMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, wireMessage{Role: string(m.Role), Content: m.Content})
	}

	body, err := json.Marshal(wireRequest{
		Model:     req.Model,
		Messages:  msgs,
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		return provider.ChatResponse{}, provider.NewProviderError(
			vendorName, provider.ErrorTypeUnknown, 0, false, "encode request", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return provider.ChatResponse{}, provider.NewProviderError(
			vendorName, provider.ErrorTypeUnknown, 0, false, "build http request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

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
	if len(parsed.Choices) == 0 {
		// OpenAI returns 200 with empty choices on some transient failures —
		// treat as a retriable server error so router can fail over.
		return provider.ChatResponse{}, provider.NewProviderError(
			vendorName, provider.ErrorTypeServer, httpResp.StatusCode, true, "empty choices", nil)
	}

	return provider.ChatResponse{
		Content:      parsed.Choices[0].Message.Content,
		FinishReason: mapFinishReason(parsed.Choices[0].FinishReason),
		Usage: provider.Usage{
			InputTokens:  parsed.Usage.PromptTokens,
			OutputTokens: parsed.Usage.CompletionTokens,
		},
		Raw: raw,
	}, nil
}

func mapFinishReason(r string) provider.FinishReason {
	switch r {
	case "stop":
		return provider.FinishStop
	case "length":
		return provider.FinishLength
	case "content_filter":
		return provider.FinishContentFilter
	case "tool_calls", "function_call":
		return provider.FinishToolUse
	default:
		return provider.FinishUnknown
	}
}

func mapTransportError(ctx context.Context, err error) *provider.ProviderError {
	// Context cancellation / deadline surfaces as a transport error here;
	// route it to ErrorTypeTimeout so router treats it as failover trigger.
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
	_ = json.Unmarshal(body, &wire) // tolerate non-JSON / empty bodies
	msg := wire.Error.Message
	if msg == "" {
		msg = "no error message"
	}

	var (
		typ       provider.ErrorType
		retriable bool
	)
	switch {
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

// parseRetryAfter handles both delta-seconds and HTTP-date formats per RFC 7231.
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

// Compile-time assertion: *Client implements provider.Provider. Placed in
// the production file (not _test.go) so `go build` catches signature drift.
var _ provider.Provider = (*Client)(nil)
