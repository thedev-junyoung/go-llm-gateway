package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider/anthropic"
)

func TestClient_KeyHash_Stable(t *testing.T) {
	t.Parallel()

	c1 := anthropic.New("sk-ant-same")
	c2 := anthropic.New("sk-ant-same")
	c3 := anthropic.New("sk-ant-different")

	if c1.KeyHash() == "" {
		t.Fatal("KeyHash() returned empty string — should always be hex SHA-256")
	}
	if c1.KeyHash() != c2.KeyHash() {
		t.Errorf("KeyHash differs for same input key: %q vs %q", c1.KeyHash(), c2.KeyHash())
	}
	if c1.KeyHash() == c3.KeyHash() {
		t.Errorf("KeyHash collides for different keys: %q", c1.KeyHash())
	}
	// SHA-256 hex digest is 64 chars.
	if len(c1.KeyHash()) != 64 {
		t.Errorf("KeyHash length = %d, want 64 (hex SHA-256)", len(c1.KeyHash()))
	}
}

// TestOption_WithModels_ReplacesDefaultSet verifies the option fully replaces
// (not merges) the default supported-model set — important for callers that
// want to lock down to a specific allowlist.
func TestOption_WithModels_ReplacesDefaultSet(t *testing.T) {
	t.Parallel()

	c := anthropic.New("sk-ant-test", anthropic.WithModels([]string{"claude-only-this-one"}))

	if !c.SupportsModel("claude-only-this-one") {
		t.Error("custom model not registered")
	}
	if c.SupportsModel("claude-opus-4-7") {
		t.Error("WithModels did not replace the default set — default model still passes")
	}
}

func TestOption_WithAPIVersion_Override(t *testing.T) {
	t.Parallel()

	var capturedVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedVersion = r.Header.Get("anthropic-version")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]any{{"type": "text", "text": "ok"}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer srv.Close()

	const customVersion = "2099-01-01"
	c := anthropic.New("sk-ant-test",
		anthropic.WithBaseURL(srv.URL),
		anthropic.WithAPIVersion(customVersion),
	)
	mt := 16
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:     "claude-opus-4-7",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: &mt,
	})
	if err != nil {
		t.Fatalf("Chat err = %v", err)
	}
	if capturedVersion != customVersion {
		t.Errorf("anthropic-version header = %q, want %q", capturedVersion, customVersion)
	}
}

// TestOption_WithAPIVersion_EmptyStringIgnored pins the documented "empty
// is ignored" behavior so callers don't accidentally clear the default.
func TestOption_WithAPIVersion_EmptyStringIgnored(t *testing.T) {
	t.Parallel()

	var capturedVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedVersion = r.Header.Get("anthropic-version")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]any{{"type": "text", "text": "ok"}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer srv.Close()

	c := anthropic.New("sk-ant-test",
		anthropic.WithBaseURL(srv.URL),
		anthropic.WithAPIVersion(""), // empty must be ignored
	)
	mt := 16
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:     "claude-opus-4-7",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: &mt,
	})
	if err != nil {
		t.Fatalf("Chat err = %v", err)
	}
	if capturedVersion == "" {
		t.Error("empty WithAPIVersion cleared the header instead of being ignored")
	}
}

// TestOption_WithHTTPClient_Injected verifies the custom client is actually
// used (not just stored). The captured transport sees the request.
func TestOption_WithHTTPClient_Injected(t *testing.T) {
	t.Parallel()

	called := false
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		body := `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Request:    r,
			Body:       readerCloserFor(body),
		}, nil
	})

	c := anthropic.New("sk-ant-test",
		anthropic.WithHTTPClient(&http.Client{Transport: rt}),
	)
	mt := 16
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:     "claude-opus-4-7",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: &mt,
	})
	if err != nil {
		t.Fatalf("Chat err = %v", err)
	}
	if !called {
		t.Error("custom RoundTripper was not invoked — WithHTTPClient option ignored")
	}
}

// TestOption_WithHTTPClient_NilIgnored guards the documented nil-safe contract
// so a caller passing a zero-value field doesn't accidentally clear the
// default *http.Client.
func TestOption_WithHTTPClient_NilIgnored(t *testing.T) {
	t.Parallel()
	// If nil were not ignored, c.http would be nil and Chat would NPE.
	// We don't actually call Chat — construction succeeding is the signal,
	// plus a follow-up Chat against a real httptest server to prove the
	// default client is still functional.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]any{{"type": "text", "text": "ok"}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer srv.Close()

	c := anthropic.New("sk-ant-test",
		anthropic.WithBaseURL(srv.URL),
		anthropic.WithHTTPClient(nil),
	)
	mt := 16
	if _, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:     "claude-opus-4-7",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: &mt,
	}); err != nil {
		t.Errorf("Chat err = %v after WithHTTPClient(nil) — nil should have been ignored", err)
	}
}

// TestChat_RetryAfterHTTPDate covers the HTTP-date branch of parseRetryAfter
// (the delta-seconds branch is already exercised by TestChat_RetryAfterHeader).
func TestChat_RetryAfterHTTPDate(t *testing.T) {
	t.Parallel()

	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", future)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`))
	}))
	defer srv.Close()

	mt := 16
	c := anthropic.New("sk-ant-test", anthropic.WithBaseURL(srv.URL))
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:     "claude-opus-4-7",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: &mt,
	})
	if err == nil {
		t.Fatal("want rate-limit error")
	}
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %v", err)
	}
	if pe.RetryAfter == nil {
		t.Fatal("RetryAfter nil — HTTP-date parsing failed silently")
	}
	if *pe.RetryAfter <= 0 || *pe.RetryAfter > 60*time.Second {
		t.Errorf("RetryAfter = %v, want in (0, 60s] from 30-second-future HTTP-date", *pe.RetryAfter)
	}
}

// TestChat_RetryAfterInvalid_NoRetryAfterField covers the parser's fallback
// when the header is neither valid delta-seconds nor HTTP-date — the error
// still surfaces, RetryAfter just stays nil.
func TestChat_RetryAfterInvalid_NoRetryAfterField(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "definitely-not-a-duration")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`))
	}))
	defer srv.Close()

	mt := 16
	c := anthropic.New("sk-ant-test", anthropic.WithBaseURL(srv.URL))
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:     "claude-opus-4-7",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: &mt,
	})

	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %v", err)
	}
	if pe.RetryAfter != nil {
		t.Errorf("RetryAfter = %v, want nil for unparseable header", pe.RetryAfter)
	}
}

// roundTripFunc adapts a function into an http.RoundTripper for tests that
// want to assert request properties without spinning up an httptest server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// readerCloserFor wraps a string body in an io.ReadCloser without dragging
// in the bytes package — keeps the test file's import set tight.
func readerCloserFor(s string) interface {
	Read(p []byte) (int, error)
	Close() error
} {
	return &stringReadCloser{r: strings.NewReader(s)}
}

type stringReadCloser struct{ r *strings.Reader }

func (s *stringReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *stringReadCloser) Close() error               { return nil }
