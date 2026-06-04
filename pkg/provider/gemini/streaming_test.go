package gemini_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider/gemini"
)

func sseBody(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	return b.String()
}

func newSSEServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
}

func collectStream(t *testing.T, stream <-chan provider.StreamChunk) []provider.StreamChunk {
	t.Helper()
	var out []provider.StreamChunk
	for chunk := range stream {
		out = append(out, chunk)
	}
	return out
}

func TestChatStream_HappyPath_CumulativeUsage(t *testing.T) {
	t.Parallel()

	body := sseBody(
		`{"candidates":[{"content":{"parts":[{"text":"Hello"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":1}}`,
		`{"candidates":[{"content":{"parts":[{"text":", "}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2}}`,
		`{"candidates":[{"content":{"parts":[{"text":"world"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3}}`,
	)
	srv := newSSEServer(t, body)
	defer srv.Close()

	c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.ChatStream(ctx, provider.ChatRequest{
		Model:    "gemini-1.5-flash",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}

	chunks := collectStream(t, stream)
	if len(chunks) != 4 {
		t.Fatalf("len(chunks) = %d, want 4 (3 content + 1 terminal)", len(chunks))
	}

	var joined strings.Builder
	for i := 0; i < 3; i++ {
		joined.WriteString(chunks[i].ContentDelta)
	}
	if got := joined.String(); got != "Hello, world" {
		t.Errorf("joined = %q, want %q", got, "Hello, world")
	}

	terminal := chunks[3]
	if terminal.FinishReason != provider.FinishStop {
		t.Errorf("terminal FinishReason = %q", terminal.FinishReason)
	}
	// Cumulative usage — terminal should hold the LAST values, not a sum.
	if terminal.Usage.InputTokens != 7 || terminal.Usage.OutputTokens != 3 {
		t.Errorf("terminal Usage = %+v, want {7, 3} (last cumulative)", terminal.Usage)
	}
}

// TestChatStream_StreamGenerateContent_URLAndAuth pins the alt=sse query
// parameter and x-goog-api-key header that the streaming path
// configures. A regression here would either flip back to JSON-array
// mode or leak the key to URL logs.
func TestChatStream_StreamGenerateContent_URLAndAuth(t *testing.T) {
	t.Parallel()

	var capturedPath, capturedQuery, capturedKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		capturedKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody(
			`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{}}`,
		)))
	}))
	defer srv.Close()

	c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
	_, err := c.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "gemini-1.5-pro",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}

	if !strings.Contains(capturedPath, "gemini-1.5-pro:streamGenerateContent") {
		t.Errorf("path = %q, want streamGenerateContent route", capturedPath)
	}
	if !strings.Contains(capturedQuery, "alt=sse") {
		t.Errorf("query = %q, want alt=sse to force SSE format", capturedQuery)
	}
	if capturedKey != "AIza-test" {
		t.Errorf("x-goog-api-key = %q, want %q", capturedKey, "AIza-test")
	}
}

func TestChatStream_PreStreamError_NoChannelAllocation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"bad key","status":"UNAUTHENTICATED"}}`))
	}))
	defer srv.Close()

	c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
	stream, err := c.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "gemini-1.5-flash",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if stream != nil {
		t.Error("stream != nil on pre-stream error")
	}
	if !errors.Is(err, provider.ErrAuthFailed) {
		t.Errorf("errors.Is(err, ErrAuthFailed) = false; got %v", err)
	}
}

func TestChatStream_UnsupportedModel(t *testing.T) {
	t.Parallel()
	c := gemini.New("AIza-test")
	_, err := c.ChatStream(context.Background(), provider.ChatRequest{
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

func TestChatStream_NegativeMaxTokens_InvalidInput(t *testing.T) {
	t.Parallel()
	c := gemini.New("AIza-test")
	mt := -1
	_, err := c.ChatStream(context.Background(), provider.ChatRequest{
		Model:     "gemini-1.5-flash",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: &mt,
	})
	var pe *provider.ProviderError
	if !errors.As(err, &pe) || pe.Type != provider.ErrorTypeInvalidInput {
		t.Fatalf("err = %v, want InvalidInput", err)
	}
}

func TestChatStream_MalformedChunk_ErrTerminal(t *testing.T) {
	t.Parallel()

	body := sseBody(
		`{"candidates":[{"content":{"parts":[{"text":"good"}]}}],"usageMetadata":{}}`,
		`{not-json-at-all`,
	)
	srv := newSSEServer(t, body)
	defer srv.Close()

	c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.ChatStream(ctx, provider.ChatRequest{
		Model:    "gemini-1.5-flash",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}

	chunks := collectStream(t, stream)
	if len(chunks) < 2 {
		t.Fatalf("len(chunks) = %d, want >= 2", len(chunks))
	}
	terminal := chunks[len(chunks)-1]
	if terminal.Err == nil {
		t.Fatal("terminal Err nil — malformed JSON must populate Err per Q7")
	}
	if terminal.FinishReason != provider.FinishUnknown {
		t.Errorf("terminal FinishReason = %q, want FinishUnknown", terminal.FinishReason)
	}
}

func TestChatStream_CtxCancel_NoErrChunk(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		mkCtx     func() (context.Context, context.CancelFunc)
		preCancel bool
	}{
		{"explicit_cancel", func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		}, true},
		{"deadline_exceeded", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 100*time.Millisecond)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			unblock := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)
				_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}],\"usageMetadata\":{}}\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
				<-unblock
			}))
			defer func() {
				close(unblock)
				srv.Close()
			}()

			c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
			ctx, cancel := tc.mkCtx()
			defer cancel()

			stream, err := c.ChatStream(ctx, provider.ChatRequest{
				Model:    "gemini-1.5-flash",
				Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("ChatStream err = %v", err)
			}

			first := <-stream
			if first.ContentDelta != "hi" {
				t.Errorf("first chunk = %q", first.ContentDelta)
			}
			if tc.preCancel {
				cancel()
			}

			deadline := time.After(2 * time.Second)
			for {
				select {
				case chunk, ok := <-stream:
					if !ok {
						return
					}
					if chunk.Err != nil {
						t.Errorf("%s produced Err chunk = %v (Q6 violation)", tc.name, chunk.Err)
					}
				case <-deadline:
					t.Fatal("channel did not close within 2s")
				}
			}
		})
	}
}

func TestChatStream_FinishReasonMapping(t *testing.T) {
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
		t.Run(fmt.Sprintf("%q", tc.raw), func(t *testing.T) {
			t.Parallel()
			body := sseBody(
				fmt.Sprintf(`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":%q}],"usageMetadata":{}}`, tc.raw),
			)
			srv := newSSEServer(t, body)
			defer srv.Close()

			c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			stream, err := c.ChatStream(ctx, provider.ChatRequest{
				Model:    "gemini-1.5-flash",
				Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("ChatStream err = %v", err)
			}
			chunks := collectStream(t, stream)
			terminal := chunks[len(chunks)-1]
			if terminal.FinishReason != tc.want {
				t.Errorf("FinishReason for %q = %q, want %q", tc.raw, terminal.FinishReason, tc.want)
			}
		})
	}
}

// TestChatStream_AssistantRoleMappedToModel pins the wire-vocabulary
// translation on the streaming path — same regression guard as the
// synchronous adapter. Producer must apply mapRoleToWire on the way
// out.
func TestChatStream_AssistantRoleMappedToModel(t *testing.T) {
	t.Parallel()

	var capturedRoles []string
	type body struct {
		Contents []struct {
			Role string `json:"role"`
		} `json:"contents"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b body
		_ = json.NewDecoder(r.Body).Decode(&b)
		for _, c := range b.Contents {
			capturedRoles = append(capturedRoles, c.Role)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody(
			`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{}}`,
		)))
	}))
	defer srv.Close()

	c := gemini.New("AIza-test", gemini.WithBaseURL(srv.URL))
	_, err := c.ChatStream(context.Background(), provider.ChatRequest{
		Model: "gemini-1.5-flash",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: "hi"},
			{Role: provider.RoleAssistant, Content: "hello back"},
			{Role: provider.RoleUser, Content: "follow-up"},
		},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}
	want := []string{"user", "model", "user"}
	if len(capturedRoles) != len(want) {
		t.Fatalf("captured %d roles, want %d", len(capturedRoles), len(want))
	}
	for i, r := range want {
		if capturedRoles[i] != r {
			t.Errorf("contents[%d].role = %q, want %q", i, capturedRoles[i], r)
		}
	}
}

func TestChatStream_SatisfiesStreamingProvider(t *testing.T) {
	t.Parallel()
	var p provider.Provider = gemini.New("AIza-test")
	if _, ok := p.(provider.StreamingProvider); !ok {
		t.Fatal("gemini.Client does not satisfy StreamingProvider")
	}
}
