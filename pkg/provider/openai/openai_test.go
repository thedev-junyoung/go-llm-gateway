package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider/openai"
)

func TestClient_Name(t *testing.T) {
	t.Parallel()
	c := openai.New("sk-test")
	if got, want := c.Name(), "openai"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

func TestClient_KeyHash_StableAndDistinct(t *testing.T) {
	t.Parallel()

	a := openai.New("sk-aaa")
	b := openai.New("sk-bbb")
	a2 := openai.New("sk-aaa")

	if a.KeyHash() == "" {
		t.Fatal("KeyHash empty")
	}
	if len(a.KeyHash()) != 64 {
		t.Errorf("KeyHash length = %d, want 64 (hex SHA-256)", len(a.KeyHash()))
	}
	if a.KeyHash() != a2.KeyHash() {
		t.Error("same key produces different hashes — not stable")
	}
	if a.KeyHash() == b.KeyHash() {
		t.Error("different keys produce the same hash — collision")
	}
	// Plaintext must not leak into the hash output.
	if strings.Contains(a.KeyHash(), "sk-aaa") {
		t.Error("KeyHash exposes plaintext key")
	}
}

func TestClient_SupportsModel(t *testing.T) {
	t.Parallel()

	c := openai.New("sk-test")
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-4o", true},
		{"gpt-4o-mini", true},
		{"gpt-3.5-turbo", true},
		{"o1-preview", true},
		{"claude-opus-4-7", false},
		{"unknown", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := c.SupportsModel(tc.model); got != tc.want {
			t.Errorf("SupportsModel(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestClient_SupportsModel_WithOverride(t *testing.T) {
	t.Parallel()

	c := openai.New("sk-test", openai.WithModels([]string{"gpt-experimental"}))
	if !c.SupportsModel("gpt-experimental") {
		t.Error("WithModels override not applied for gpt-experimental")
	}
	if c.SupportsModel("gpt-4o") {
		t.Error("WithModels should replace, not extend the default set")
	}
}

func TestChat_HappyPath(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/chat/completions"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer sk-test"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}

		body, _ := io.ReadAll(r.Body)
		// Verify system prompt is prepended and user message follows.
		if !strings.Contains(string(body), `"role":"system","content":"sys"`) {
			t.Errorf("system message missing, body=%s", body)
		}
		if !strings.Contains(string(body), `"role":"user","content":"hi"`) {
			t.Errorf("user message missing, body=%s", body)
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"content": "hello back"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{
				"prompt_tokens":     12,
				"completion_tokens": 3,
			},
		})
	}))
	defer server.Close()

	c := openai.New("sk-test", openai.WithBaseURL(server.URL))
	resp, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		System:   "sys",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat() err = %v", err)
	}
	if resp.Content != "hello back" {
		t.Errorf("Content = %q, want %q", resp.Content, "hello back")
	}
	if resp.FinishReason != provider.FinishStop {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, provider.FinishStop)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v, want {12 3}", resp.Usage)
	}
	if len(resp.Raw) == 0 {
		t.Error("Raw is empty; should contain original response body")
	}
}

func TestChat_OmitsMaxTokensWhenNil(t *testing.T) {
	t.Parallel()

	var captured string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = string(body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer server.Close()

	c := openai.New("sk-test", openai.WithBaseURL(server.URL))
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat() err = %v", err)
	}
	if strings.Contains(captured, "max_tokens") {
		t.Errorf("max_tokens should be absent when MaxTokens is nil; body=%s", captured)
	}
}

func TestChat_IncludesMaxTokensWhenSet(t *testing.T) {
	t.Parallel()

	var captured string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = string(body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer server.Close()

	c := openai.New("sk-test", openai.WithBaseURL(server.URL))
	maxTok := 256
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:     "gpt-4o",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: &maxTok,
	})
	if err != nil {
		t.Fatalf("Chat() err = %v", err)
	}
	if !strings.Contains(captured, `"max_tokens":256`) {
		t.Errorf("max_tokens=256 missing from body=%s", captured)
	}
}

func TestChat_UnsupportedModel(t *testing.T) {
	t.Parallel()

	c := openai.New("sk-test")
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "claude-opus-4-7",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})

	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %T", err)
	}
	if pe.Type != provider.ErrorTypeInvalidInput {
		t.Errorf("Type = %q, want %q", pe.Type, provider.ErrorTypeInvalidInput)
	}
}

func TestChat_ErrorMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		status    int
		wantType  provider.ErrorType
		wantRetry bool
		sentinel  error // optional — pass nil to skip errors.Is check
	}{
		{"429 rate_limit", 429, provider.ErrorTypeRateLimit, true, provider.ErrRateLimited},
		{"401 auth", 401, provider.ErrorTypeAuth, false, provider.ErrAuthFailed},
		{"403 permission", 403, provider.ErrorTypePermission, false, nil},
		{"404 not_found", 404, provider.ErrorTypeNotFound, false, nil},
		{"400 invalid", 400, provider.ErrorTypeInvalidInput, false, nil},
		{"500 server", 500, provider.ErrorTypeServer, true, nil},
		{"503 server", 503, provider.ErrorTypeServer, true, nil},
		{"418 unknown", 418, provider.ErrorTypeUnknown, false, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"some_type"}}`))
			}))
			defer server.Close()

			c := openai.New("sk-test", openai.WithBaseURL(server.URL))
			_, err := c.Chat(context.Background(), provider.ChatRequest{
				Model:    "gpt-4o",
				Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
			})

			var pe *provider.ProviderError
			if !errors.As(err, &pe) {
				t.Fatalf("err is not *ProviderError: %T", err)
			}
			if pe.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", pe.Type, tc.wantType)
			}
			if pe.Retriable != tc.wantRetry {
				t.Errorf("Retriable = %v, want %v", pe.Retriable, tc.wantRetry)
			}
			if pe.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", pe.StatusCode, tc.status)
			}
			if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
				t.Errorf("errors.Is(err, %v) = false, want true", tc.sentinel)
			}
		})
	}
}

func TestChat_RetryAfterHeader(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer server.Close()

	c := openai.New("sk-test", openai.WithBaseURL(server.URL))
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})

	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %T", err)
	}
	if pe.RetryAfter == nil {
		t.Fatal("RetryAfter = nil, want 7s")
	}
	if got := *pe.RetryAfter; got != 7*time.Second {
		t.Errorf("RetryAfter = %v, want %v", got, 7*time.Second)
	}
}

func TestChat_ContextCanceled(t *testing.T) {
	t.Parallel()

	// Pre-cancelled context: deterministic, no sleeps, no inter-goroutine
	// races. Validates the same mapTransportError path the production
	// router relies on — Chat() must surface ctx.Err() as ErrorTypeTimeout.
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer server.Close()

	c := openai.New("sk-test", openai.WithBaseURL(server.URL))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Chat(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})

	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %T", err)
	}
	if pe.Type != provider.ErrorTypeTimeout {
		t.Errorf("Type = %q, want %q", pe.Type, provider.ErrorTypeTimeout)
	}
	if !errors.Is(err, provider.ErrTimeout) {
		t.Error("errors.Is(err, ErrTimeout) = false, want true")
	}
}

func TestChat_FinishReasonMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw  string
		want provider.FinishReason
	}{
		{"stop", provider.FinishStop},
		{"length", provider.FinishLength},
		{"content_filter", provider.FinishContentFilter},
		{"tool_calls", provider.FinishToolUse},
		{"function_call", provider.FinishToolUse},
		{"never_heard_of_this", provider.FinishUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{
						"message":       map[string]any{"content": "x"},
						"finish_reason": tc.raw,
					}},
					"usage": map[string]int{},
				})
			}))
			defer server.Close()

			c := openai.New("sk-test", openai.WithBaseURL(server.URL))
			resp, err := c.Chat(context.Background(), provider.ChatRequest{
				Model:    "gpt-4o",
				Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("Chat() err = %v", err)
			}
			if resp.FinishReason != tc.want {
				t.Errorf("FinishReason for %q = %q, want %q", tc.raw, resp.FinishReason, tc.want)
			}
		})
	}
}

func TestChat_EmptyChoicesRetriable(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[],"usage":{}}`))
	}))
	defer server.Close()

	c := openai.New("sk-test", openai.WithBaseURL(server.URL))
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})

	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %T", err)
	}
	if pe.Type != provider.ErrorTypeServer || !pe.Retriable {
		t.Errorf("got Type=%q Retriable=%v, want server / true", pe.Type, pe.Retriable)
	}
}
