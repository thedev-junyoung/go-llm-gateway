// Package gemini implements provider.Provider against Google's
// Generative Language API (Gemini). The wire format quirks worth pinning
// up front:
//
//   - The model id is part of the URL path, not the JSON body
//     (POST /v1beta/models/{model}:generateContent).
//   - The assistant role is wire-named "model" — adapter translates from
//     provider.RoleAssistant.
//   - The system prompt rides on a top-level systemInstruction field, the
//     same shape Anthropic uses.
//   - max_tokens is optional; passing nil through preserves the vendor
//     default (matches OpenAI's behavior, differs from Anthropic which
//     requires a value).
//
// See docs/adr/0002-provider-interface-design.md for the cross-vendor
// mapping rules.
package gemini

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
	"net/url"
	"strings"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

const (
	vendorName     = "gemini"
	defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"
	defaultTimeout = 60 * time.Second

	// roleAssistantWire is Gemini's name for the assistant role. The
	// adapter translates provider.RoleAssistant <-> "model" so callers
	// never see this string.
	roleAssistantWire = "model"
)

// defaultModels enumerates the Gemini models the adapter recognizes at
// build time. Use WithModels for early-access / preview ids without an
// adapter release.
var defaultModels = map[string]struct{}{
	"gemini-2.0-flash-exp": {},
	"gemini-1.5-pro":       {},
	"gemini-1.5-flash":     {},
}

// Client is the Gemini adapter.
type Client struct {
	apiKey  string
	keyHash string // computed once in New so KeyHash stays allocation-free
	baseURL string
	http    *http.Client
	models  map[string]struct{}
}

// Option configures Client at construction time.
type Option func(*Client)

// WithBaseURL points the client at a non-default endpoint (proxy,
// httptest server, regional Vertex AI Express endpoint). Default is the
// public generativelanguage.googleapis.com API.
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

// New constructs a Gemini provider client. apiKey is required.
func New(apiKey string, opts ...Option) *Client {
	sum := sha256.Sum256([]byte(apiKey))
	c := &Client{
		apiKey:  apiKey,
		keyHash: hex.EncodeToString(sum[:]),
		baseURL: defaultBaseURL,
		http:    &http.Client{Timeout: defaultTimeout},
		models:  defaultModels,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Name returns "gemini".
func (c *Client) Name() string { return vendorName }

// SupportsModel reports whether this client can serve the given model id.
func (c *Client) SupportsModel(model string) bool {
	_, ok := c.models[model]
	return ok
}

// KeyHash returns a stable hex SHA-256 of the configured API key.
func (c *Client) KeyHash() string { return c.keyHash }

// Wire types. Package-private — callers go through provider.ChatRequest /
// provider.ChatResponse only.

type wirePart struct {
	Text string `json:"text"`
}

type wireContent struct {
	Role  string     `json:"role,omitempty"`
	Parts []wirePart `json:"parts"`
}

type wireGenerationConfig struct {
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

type wireRequest struct {
	Contents          []wireContent         `json:"contents"`
	SystemInstruction *wireContent          `json:"systemInstruction,omitempty"`
	GenerationConfig  *wireGenerationConfig `json:"generationConfig,omitempty"`
}

type wireCandidate struct {
	Content      wireContent `json:"content"`
	FinishReason string      `json:"finishReason"`
}

type wireUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

type wireResponse struct {
	Candidates    []wireCandidate   `json:"candidates"`
	UsageMetadata wireUsageMetadata `json:"usageMetadata"`
}

type wireError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// Chat issues a generateContent request and maps the response into the
// gateway-neutral provider.ChatResponse / *provider.ProviderError types.
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	if !c.SupportsModel(req.Model) {
		return provider.ChatResponse{}, provider.NewProviderError(
			vendorName, provider.ErrorTypeInvalidInput, 0, false,
			fmt.Sprintf("unsupported model %q", req.Model), nil)
	}

	contents := make([]wireContent, 0, len(req.Messages))
	for _, m := range req.Messages {
		contents = append(contents, wireContent{
			Role:  mapRoleToWire(m.Role),
			Parts: []wirePart{{Text: m.Content}},
		})
	}

	wreq := wireRequest{Contents: contents}
	if req.System != "" {
		wreq.SystemInstruction = &wireContent{
			Parts: []wirePart{{Text: req.System}},
		}
	}
	if req.MaxTokens != nil {
		if *req.MaxTokens <= 0 {
			return provider.ChatResponse{}, provider.NewProviderError(
				vendorName, provider.ErrorTypeInvalidInput, 0, false,
				"MaxTokens must be > 0", nil)
		}
		wreq.GenerationConfig = &wireGenerationConfig{MaxOutputTokens: *req.MaxTokens}
	}

	body, err := json.Marshal(wreq)
	if err != nil {
		return provider.ChatResponse{}, provider.NewProviderError(
			vendorName, provider.ErrorTypeUnknown, 0, false, "encode request", err)
	}

	endpoint := c.baseURL + "/models/" + url.PathEscape(req.Model) + ":generateContent"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return provider.ChatResponse{}, provider.NewProviderError(
			vendorName, provider.ErrorTypeUnknown, 0, false, "build http request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// API key on the header (x-goog-api-key) instead of the query param
	// keeps the key out of any URL logs the user's reverse proxy or
	// browser dev-tools might capture. Functionally identical to ?key=.
	httpReq.Header.Set("x-goog-api-key", c.apiKey)

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

	content := ""
	finish := provider.FinishUnknown
	if len(parsed.Candidates) > 0 {
		first := parsed.Candidates[0]
		content = joinTextParts(first.Content.Parts)
		finish = mapFinishReason(first.FinishReason)
	}

	return provider.ChatResponse{
		Content:      content,
		FinishReason: finish,
		Usage: provider.Usage{
			InputTokens:  parsed.UsageMetadata.PromptTokenCount,
			OutputTokens: parsed.UsageMetadata.CandidatesTokenCount,
		},
		Raw: raw,
	}, nil
}

// mapRoleToWire translates the gateway-neutral Role into Gemini's wire
// vocabulary — the only non-trivial mapping is RoleAssistant → "model".
func mapRoleToWire(r provider.Role) string {
	if r == provider.RoleAssistant {
		return roleAssistantWire
	}
	return string(r)
}

// joinTextParts concatenates the text parts of a candidate's content with
// "\n" per the gateway contract (ADR-002 ChatResponse.Content). Non-text
// parts (function_call, inline_data, file_data) are intentionally dropped
// — callers reach for ChatResponse.Raw.
func joinTextParts(parts []wirePart) string {
	var b strings.Builder
	first := true
	for _, p := range parts {
		if p.Text == "" {
			continue
		}
		if !first {
			b.WriteByte('\n')
		}
		b.WriteString(p.Text)
		first = false
	}
	return b.String()
}

// mapFinishReason translates Gemini's finishReason enum into the gateway
// vocabulary. SAFETY and RECITATION are Gemini-specific signals — they
// surface as FinishContentFilter (the closest neutral concept) so callers
// who care about the distinction parse Raw.
func mapFinishReason(r string) provider.FinishReason {
	switch r {
	case "STOP":
		return provider.FinishStop
	case "MAX_TOKENS":
		return provider.FinishLength
	case "SAFETY", "RECITATION":
		return provider.FinishContentFilter
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

	return provider.NewProviderError(vendorName, typ, resp.StatusCode, retriable, msg, nil)
}

// Compile-time interface satisfaction.
var _ provider.Provider = (*Client)(nil)
