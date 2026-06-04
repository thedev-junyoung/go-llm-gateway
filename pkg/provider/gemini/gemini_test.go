package gemini_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider/gemini"
)

func TestClient_Name(t *testing.T) {
	t.Parallel()
	c := gemini.New("AIza-test")
	if got, want := c.Name(), "gemini"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

func TestClient_SupportsModel(t *testing.T) {
	t.Parallel()

	c := gemini.New("AIza-test")
	cases := []struct {
		model string
		want  bool
	}{
		{"gemini-2.0-flash-exp", true},
		{"gemini-1.5-pro", true},
		{"gemini-1.5-flash", true},
		{"gpt-4o", false},
		{"claude-opus-4-7", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := c.SupportsModel(tc.model); got != tc.want {
			t.Errorf("SupportsModel(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestClient_KeyHash_Stable(t *testing.T) {
	t.Parallel()

	c1 := gemini.New("AIza-same")
	c2 := gemini.New("AIza-same")
	c3 := gemini.New("AIza-different")

	if c1.KeyHash() == "" {
		t.Fatal("KeyHash() returned empty string")
	}
	if c1.KeyHash() != c2.KeyHash() {
		t.Errorf("KeyHash differs for same input")
	}
	if c1.KeyHash() == c3.KeyHash() {
		t.Errorf("KeyHash collides for different keys")
	}
	if len(c1.KeyHash()) != 64 {
		t.Errorf("KeyHash length = %d, want 64", len(c1.KeyHash()))
	}
}

func TestChat_HappyPath_SystemAndMessages(t *testing.T) {
	t.Parallel()

	var captured wireRequestForTest
	var capturedPath, capturedAPIKey string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedAPIKey = r.Header.Get("x-goog-api-key")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{
				{
					"content": map[string]any{
						"role":  "model",
						"parts": []map[string]any{{"text": "Because of Rayleigh scattering."}},
					},
					"finishReason": "STOP",
				},
			},
			"usageMetadata": map[string]int{
				"promptTokenCount":     12,
				"candidatesTokenCount": 7,
				"totalTokenCount":      19,
			},
		})
	}))
	defer srv.Close()

	c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
	mt := 64
	resp, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:  "gemini-2.0-flash-exp",
		System: "You answer in one sentence.",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: "Why is the sky blue?"},
		},
		MaxTokens: &mt,
	})
	if err != nil {
		t.Fatalf("Chat err = %v", err)
	}

	if resp.Content != "Because of Rayleigh scattering." {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.FinishReason != provider.FinishStop {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, provider.FinishStop)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 7 {
		t.Errorf("Usage = %+v, want {12, 7}", resp.Usage)
	}
	if !strings.Contains(capturedPath, "gemini-2.0-flash-exp:generateContent") {
		t.Errorf("path = %q, want it to contain model:generateContent", capturedPath)
	}
	if capturedAPIKey != "AIza-test" {
		t.Errorf("x-goog-api-key = %q, want %q", capturedAPIKey, "AIza-test")
	}
	if captured.SystemInstruction == nil || len(captured.SystemInstruction.Parts) == 0 {
		t.Fatal("system_instruction not sent")
	}
	if captured.SystemInstruction.Parts[0].Text != "You answer in one sentence." {
		t.Errorf("system text = %q", captured.SystemInstruction.Parts[0].Text)
	}
	if captured.GenerationConfig == nil || captured.GenerationConfig.MaxOutputTokens != 64 {
		t.Errorf("generationConfig.maxOutputTokens missing or wrong: %+v", captured.GenerationConfig)
	}
}

// TestChat_RoleAssistantMappedToModel pins the wire vocabulary translation.
// A regression here would silently change the conversation order Gemini sees.
func TestChat_RoleAssistantMappedToModel(t *testing.T) {
	t.Parallel()

	var captured wireRequestForTest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{}}`))
	}))
	defer srv.Close()

	c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model: "gemini-1.5-pro",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: "hi"},
			{Role: provider.RoleAssistant, Content: "hello back"},
			{Role: provider.RoleUser, Content: "follow-up"},
		},
	})
	if err != nil {
		t.Fatalf("Chat err = %v", err)
	}

	if len(captured.Contents) != 3 {
		t.Fatalf("contents len = %d, want 3", len(captured.Contents))
	}
	if captured.Contents[0].Role != "user" {
		t.Errorf("contents[0].role = %q, want user", captured.Contents[0].Role)
	}
	if captured.Contents[1].Role != "model" {
		t.Errorf("contents[1].role = %q, want model (assistant → model)", captured.Contents[1].Role)
	}
	if captured.Contents[2].Role != "user" {
		t.Errorf("contents[2].role = %q, want user", captured.Contents[2].Role)
	}
}

func TestChat_MultiPartResponseJoin(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"line one"},{"text":"line two"}]},"finishReason":"STOP"}],"usageMetadata":{}}`))
	}))
	defer srv.Close()

	c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
	resp, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "gemini-1.5-flash",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat err = %v", err)
	}
	if resp.Content != "line one\nline two" {
		t.Errorf("multi-part join = %q, want %q", resp.Content, "line one\nline two")
	}
}

func TestChat_NilMaxTokens_NoGenerationConfig(t *testing.T) {
	t.Parallel()

	var captured wireRequestForTest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{}}`))
	}))
	defer srv.Close()

	c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "gemini-1.5-flash",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		// MaxTokens nil — vendor default expected
	})
	if err != nil {
		t.Fatalf("Chat err = %v", err)
	}
	if captured.GenerationConfig != nil {
		t.Errorf("generationConfig sent = %+v, want nil (let vendor default)", captured.GenerationConfig)
	}
}

func TestChat_NegativeMaxTokens_InvalidInput(t *testing.T) {
	t.Parallel()

	c := gemini.New("AIza-test")
	mt := -1
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:     "gemini-1.5-flash",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: &mt,
	})

	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %v", err)
	}
	if pe.Type != provider.ErrorTypeInvalidInput {
		t.Errorf("Type = %q, want %q", pe.Type, provider.ErrorTypeInvalidInput)
	}
}

func TestChat_UnsupportedModel(t *testing.T) {
	t.Parallel()

	c := gemini.New("AIza-test")
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %v", err)
	}
	if pe.Type != provider.ErrorTypeInvalidInput {
		t.Errorf("Type = %q, want %q", pe.Type, provider.ErrorTypeInvalidInput)
	}
}

func TestChat_ErrorMapping(t *testing.T) {
	t.Parallel()

	// Subtest name uses status code (not Type) because two cases share the
	// same Type — 500/503 both map to ErrorTypeServer.
	cases := []struct {
		status    int
		body      string
		wantType  provider.ErrorType
		retriable bool
		sentinel  error
		matchVend string
	}{
		{429, `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`, provider.ErrorTypeRateLimit, true, provider.ErrRateLimited, "gemini"},
		{401, `{"error":{"code":401,"message":"bad key","status":"UNAUTHENTICATED"}}`, provider.ErrorTypeAuth, false, provider.ErrAuthFailed, "gemini"},
		{403, `{"error":{"code":403,"message":"forbidden","status":"PERMISSION_DENIED"}}`, provider.ErrorTypePermission, false, nil, "gemini"},
		{404, `{"error":{"code":404,"message":"not found","status":"NOT_FOUND"}}`, provider.ErrorTypeNotFound, false, nil, "gemini"},
		{400, `{"error":{"code":400,"message":"bad request","status":"INVALID_ARGUMENT"}}`, provider.ErrorTypeInvalidInput, false, nil, "gemini"},
		{500, `{"error":{"code":500,"message":"server","status":"INTERNAL"}}`, provider.ErrorTypeServer, true, nil, "gemini"},
		{503, `{"error":{"code":503,"message":"down","status":"UNAVAILABLE"}}`, provider.ErrorTypeServer, true, nil, "gemini"},
		{418, `{"error":{"code":418,"message":"teapot"}}`, provider.ErrorTypeUnknown, false, nil, "gemini"},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("status_%d", tc.status), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
			_, err := c.Chat(context.Background(), provider.ChatRequest{
				Model:    "gemini-1.5-flash",
				Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
			})
			var pe *provider.ProviderError
			if !errors.As(err, &pe) {
				t.Fatalf("err is not *ProviderError: %v", err)
			}
			if pe.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", pe.Type, tc.wantType)
			}
			if pe.Retriable != tc.retriable {
				t.Errorf("Retriable = %v, want %v", pe.Retriable, tc.retriable)
			}
			if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
				t.Errorf("errors.Is(err, sentinel) = false, want true for %v", tc.sentinel)
			}
			if pe.Vendor() != tc.matchVend {
				t.Errorf("Vendor() = %q, want %q", pe.Vendor(), tc.matchVend)
			}
		})
	}
}

// TestChat_RetryAfterHeader_DeltaSeconds pins the same Retry-After contract
// the openai and anthropic adapters already provide — a 429 with a vendor
// backoff hint MUST surface on *ProviderError.RetryAfter so the router can
// honor it instead of dog-piling the vendor.
func TestChat_RetryAfterHeader_DeltaSeconds(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"slow"}}`))
	}))
	defer srv.Close()

	c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "gemini-1.5-flash",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %v", err)
	}
	if pe.RetryAfter == nil {
		t.Fatal("RetryAfter nil — 429 + Retry-After header should attach the hint")
	}
	if *pe.RetryAfter != 42*time.Second {
		t.Errorf("RetryAfter = %v, want 42s", *pe.RetryAfter)
	}
}

func TestChat_RetryAfterHeader_HTTPDate(t *testing.T) {
	t.Parallel()

	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", future)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"slow"}}`))
	}))
	defer srv.Close()

	c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "gemini-1.5-flash",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %v", err)
	}
	if pe.RetryAfter == nil {
		t.Fatal("RetryAfter nil — HTTP-date Retry-After should parse")
	}
	if *pe.RetryAfter <= 0 || *pe.RetryAfter > 60*time.Second {
		t.Errorf("RetryAfter = %v, want in (0, 60s] from 30-second-future HTTP-date", *pe.RetryAfter)
	}
}

func TestChat_RetryAfterHeader_Invalid_NoField(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "definitely-not-a-duration")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"slow"}}`))
	}))
	defer srv.Close()

	c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
	_, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "gemini-1.5-flash",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %v", err)
	}
	if pe.RetryAfter != nil {
		t.Errorf("RetryAfter = %v, want nil for unparseable header", pe.RetryAfter)
	}
}

func TestChat_ContextCanceled(t *testing.T) {
	t.Parallel()

	// unblock is the handler's "the caller cancelled, stop pretending to
	// be slow" signal — the previous sleep-based implementation made every
	// run of this test take 500ms regardless of how fast the client
	// aborted. With this channel the test finishes the moment the client
	// times out and we just unblock the handler to let srv.Close return.
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-unblock
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
	_, err := c.Chat(ctx, provider.ChatRequest{
		Model:    "gemini-1.5-flash",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("errors.Is(err, ErrTimeout) = false, want true; got %v", err)
	}
}

func TestChat_FinishReasonMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw  string
		want provider.FinishReason
	}{
		{"STOP", provider.FinishStop},
		{"MAX_TOKENS", provider.FinishLength},
		{"SAFETY", provider.FinishContentFilter},
		{"RECITATION", provider.FinishContentFilter},
		{"OTHER", provider.FinishUnknown},
		{"FINISH_REASON_UNSPECIFIED", provider.FinishUnknown},
		{"", provider.FinishUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"` + tc.raw + `"}],"usageMetadata":{}}`))
			}))
			defer srv.Close()

			c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
			resp, err := c.Chat(context.Background(), provider.ChatRequest{
				Model:    "gemini-1.5-flash",
				Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("Chat err = %v", err)
			}
			if resp.FinishReason != tc.want {
				t.Errorf("FinishReason for %q = %q, want %q", tc.raw, resp.FinishReason, tc.want)
			}
		})
	}
}

// wireRequestForTest mirrors the adapter's wire shape closely enough to
// assert request body structure without importing the adapter's internals.
type wireRequestForTest struct {
	Contents []struct {
		Role  string `json:"role"`
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"contents"`
	SystemInstruction *struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"systemInstruction,omitempty"`
	GenerationConfig *struct {
		MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
	} `json:"generationConfig,omitempty"`
}
