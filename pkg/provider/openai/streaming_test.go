package openai_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider/openai"
)

// sseBody is a small helper that builds an OpenAI-shaped SSE response
// body. Each frame is `data: <json>\n\n`. The final `data: [DONE]\n\n`
// terminator triggers the terminal chunk emission.
func sseBody(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
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

func collect(t *testing.T, stream <-chan provider.StreamChunk) []provider.StreamChunk {
	t.Helper()
	var out []provider.StreamChunk
	for chunk := range stream {
		out = append(out, chunk)
	}
	return out
}

func TestChatStream_HappyPath_DeltaAndUsage(t *testing.T) {
	t.Parallel()

	body := sseBody(
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"content":"Hello"}}]}`,
		`{"choices":[{"delta":{"content":", "}}]}`,
		`{"choices":[{"delta":{"content":"world"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
	)
	srv := newSSEServer(t, body)
	defer srv.Close()

	c := openai.New("sk-test", openai.WithBaseURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.ChatStream(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}

	chunks := collect(t, stream)
	if len(chunks) != 4 {
		t.Fatalf("len(chunks) = %d, want 4 (3 content + 1 terminal)", len(chunks))
	}

	var joined strings.Builder
	for i := 0; i < 3; i++ {
		joined.WriteString(chunks[i].ContentDelta)
		if chunks[i].FinishReason != "" {
			t.Errorf("intermediate chunk %d has FinishReason = %q, want empty", i, chunks[i].FinishReason)
		}
	}
	if got := joined.String(); got != "Hello, world" {
		t.Errorf("joined content = %q, want %q", got, "Hello, world")
	}

	terminal := chunks[3]
	if terminal.FinishReason != provider.FinishStop {
		t.Errorf("terminal FinishReason = %q, want %q", terminal.FinishReason, provider.FinishStop)
	}
	if terminal.Usage.InputTokens != 7 || terminal.Usage.OutputTokens != 3 {
		t.Errorf("terminal Usage = %+v, want {7, 3}", terminal.Usage)
	}
	if terminal.ContentDelta != "" {
		t.Errorf("terminal ContentDelta = %q, want empty", terminal.ContentDelta)
	}
	if terminal.Err != nil {
		t.Errorf("terminal Err = %v, want nil", terminal.Err)
	}
}

// TestChatStream_PreStreamError_NoChannelAllocation pins ADR-007 Q4's
// "pre-stream errors return (nil, *ProviderError)" contract — caller
// can branch on err without allocating a goroutine.
func TestChatStream_PreStreamError_NoChannelAllocation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key","type":"invalid_api_key"}}`))
	}))
	defer srv.Close()

	c := openai.New("sk-test", openai.WithBaseURL(srv.URL))
	stream, err := c.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})

	if stream != nil {
		t.Error("stream != nil on pre-stream error — should not allocate channel")
	}
	if !errors.Is(err, provider.ErrAuthFailed) {
		t.Errorf("errors.Is(err, ErrAuthFailed) = false, want true; got %v", err)
	}
}

func TestChatStream_UnsupportedModel(t *testing.T) {
	t.Parallel()

	c := openai.New("sk-test")
	_, err := c.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "gemini-2.0-flash-exp",
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

// TestChatStream_MidStreamMalformedJSON pins the Q7 contract: a chunk
// that fails to decode produces a terminal chunk with Err set, then
// channel close.
func TestChatStream_MidStreamMalformedJSON(t *testing.T) {
	t.Parallel()

	body := "data: {\"choices\":[{\"delta\":{\"content\":\"good\"}}]}\n\n" +
		"data: {not-json-at-all\n\n" +
		"data: [DONE]\n\n"
	srv := newSSEServer(t, body)
	defer srv.Close()

	c := openai.New("sk-test", openai.WithBaseURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.ChatStream(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}

	chunks := collect(t, stream)
	if len(chunks) < 2 {
		t.Fatalf("len(chunks) = %d, want >= 2 (good delta + error terminal)", len(chunks))
	}
	terminal := chunks[len(chunks)-1]
	if terminal.Err == nil {
		t.Fatal("terminal chunk Err nil — malformed JSON should populate Err per Q7")
	}
	if terminal.FinishReason != provider.FinishUnknown {
		t.Errorf("terminal FinishReason = %q, want FinishUnknown when Err set", terminal.FinishReason)
	}
}

// TestChatStream_CtxCancel_NoErrChunk pins ADR-007 Q6: a ctx-cancel
// terminates the stream via channel close, NOT via an Err chunk.
// Caller checks ctx.Err() to distinguish cancellation from normal end.
func TestChatStream_CtxCancel_NoErrChunk(t *testing.T) {
	t.Parallel()

	// Slow server: emit one chunk then hold the connection open
	// indefinitely. The unblock channel lets srv.Close() return after
	// the test finishes.
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		<-unblock
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()

	c := openai.New("sk-test", openai.WithBaseURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.ChatStream(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}

	// Read one chunk, then cancel.
	got := <-stream
	if got.ContentDelta != "hi" {
		t.Errorf("first chunk content = %q, want %q", got.ContentDelta, "hi")
	}
	cancel()

	// Drain. We accept channel close within 2 seconds; any chunks that
	// arrive must NOT be Err chunks (Q6: ctx-cancel doesn't emit Err).
	deadline := time.After(2 * time.Second)
	for {
		select {
		case chunk, ok := <-stream:
			if !ok {
				return // closed — good
			}
			if chunk.Err != nil {
				t.Errorf("ctx-cancel produced Err chunk = %v, want channel close only", chunk.Err)
			}
		case <-deadline:
			t.Fatal("channel did not close within 2s of ctx cancel")
		}
	}
}

// TestChatStream_CtxDeadline_NoErrChunk is the Q6 counterpart to the
// explicit-cancel test — DeadlineExceeded follows the same close-only
// path. Earlier implementation only excluded context.Canceled from
// the Err-chunk emission, leaking an Err chunk on WithTimeout expiry;
// this test pins that gap closed.
func TestChatStream_CtxDeadline_NoErrChunk(t *testing.T) {
	t.Parallel()

	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		<-unblock
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()

	c := openai.New("sk-test", openai.WithBaseURL(srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	stream, err := c.ChatStream(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}

	// Drain the channel until it closes. Any Err chunk would be a Q6
	// violation — DeadlineExceeded must mirror Canceled's close-only
	// semantics.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case chunk, ok := <-stream:
			if !ok {
				return
			}
			if chunk.Err != nil {
				t.Errorf("ctx-timeout produced Err chunk = %v, want channel close only", chunk.Err)
			}
		case <-deadline:
			t.Fatal("channel did not close within 2s of ctx timeout")
		}
	}
}

// TestChatStream_KeepAliveAndCommentLinesIgnored pins SSE-spec tolerance:
// blank lines and `: comment` lines are ignored without affecting the
// chunk stream.
func TestChatStream_KeepAliveAndCommentLinesIgnored(t *testing.T) {
	t.Parallel()

	body := ": keep-alive comment\n\n" +
		"\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"only\"}}]}\n\n" +
		": another comment\n\n" +
		"data: [DONE]\n\n"
	srv := newSSEServer(t, body)
	defer srv.Close()

	c := openai.New("sk-test", openai.WithBaseURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := c.ChatStream(ctx, provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}

	chunks := collect(t, stream)
	if len(chunks) != 2 {
		t.Fatalf("len(chunks) = %d, want 2 (one content + one terminal)", len(chunks))
	}
	if chunks[0].ContentDelta != "only" {
		t.Errorf("ContentDelta = %q", chunks[0].ContentDelta)
	}
}

func TestChatStream_FinishReasonMapping(t *testing.T) {
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
		{"", provider.FinishUnknown},
		{"surprise_new_value", provider.FinishUnknown},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.raw), func(t *testing.T) {
			t.Parallel()
			body := sseBody(
				`{"choices":[{"delta":{"content":"x"}}]}`,
				fmt.Sprintf(`{"choices":[{"delta":{},"finish_reason":%q}]}`, tc.raw),
			)
			srv := newSSEServer(t, body)
			defer srv.Close()

			c := openai.New("sk-test", openai.WithBaseURL(srv.URL))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			stream, err := c.ChatStream(ctx, provider.ChatRequest{
				Model:    "gpt-4o",
				Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("ChatStream err = %v", err)
			}
			chunks := collect(t, stream)
			terminal := chunks[len(chunks)-1]
			if terminal.FinishReason != tc.want {
				t.Errorf("FinishReason for %q = %q, want %q", tc.raw, terminal.FinishReason, tc.want)
			}
		})
	}
}

// TestChatStream_TypeAssertion_SatisfiesStreamingProvider pins the
// interface satisfaction at the package boundary — duplicates the
// compile-time var in streaming.go but at the test level so a future
// type rename without the assertion update fails loudly.
func TestChatStream_TypeAssertion_SatisfiesStreamingProvider(t *testing.T) {
	t.Parallel()
	var p provider.Provider = openai.New("sk-test")
	if _, ok := p.(provider.StreamingProvider); !ok {
		t.Fatal("openai.Client does not satisfy StreamingProvider")
	}
}
