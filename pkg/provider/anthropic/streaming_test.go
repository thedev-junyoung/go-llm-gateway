package anthropic_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider/anthropic"
)

// sseEvent builds one event: <name>\ndata: <data>\n\n frame.
func sseEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

// happyStreamBody is the standard message lifecycle Anthropic emits on
// a successful completion: message_start → content_block_start →
// content_block_delta(s) → content_block_stop → message_delta →
// message_stop. Most tests build off variants of this.
func happyStreamBody(deltas []string, stopReason string) string {
	var b strings.Builder
	b.WriteString(sseEvent("message_start",
		`{"type":"message_start","message":{"usage":{"input_tokens":7,"output_tokens":0}}}`))
	b.WriteString(sseEvent("content_block_start",
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	for _, d := range deltas {
		b.WriteString(sseEvent("content_block_delta",
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+d+`"}}`))
	}
	b.WriteString(sseEvent("content_block_stop",
		`{"type":"content_block_stop","index":0}`))
	b.WriteString(sseEvent("message_delta",
		`{"type":"message_delta","delta":{"stop_reason":"`+stopReason+`"},"usage":{"output_tokens":3}}`))
	b.WriteString(sseEvent("message_stop", `{"type":"message_stop"}`))
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

func TestChatStream_HappyPath_DeltaUsageAndStopReason(t *testing.T) {
	t.Parallel()

	srv := newSSEServer(t, happyStreamBody([]string{"Hello", ", ", "world"}, "end_turn"))
	defer srv.Close()

	c := anthropic.New("sk-ant-test", anthropic.WithBaseURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mt := 64
	stream, err := c.ChatStream(ctx, provider.ChatRequest{
		Model:     "claude-opus-4-7",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: &mt,
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
		t.Errorf("joined = %q, want %q", got, "Hello, world")
	}

	terminal := chunks[3]
	if terminal.FinishReason != provider.FinishStop {
		t.Errorf("terminal FinishReason = %q, want %q", terminal.FinishReason, provider.FinishStop)
	}
	if terminal.Usage.InputTokens != 7 || terminal.Usage.OutputTokens != 3 {
		t.Errorf("terminal Usage = %+v, want {7, 3}", terminal.Usage)
	}
	if terminal.Err != nil {
		t.Errorf("terminal Err = %v, want nil", terminal.Err)
	}
}

// TestChatStream_UsageSplitAcrossEvents pins the wire quirk that
// input_tokens arrive on message_start and output_tokens arrive on
// message_delta — the producer must accumulate from both.
func TestChatStream_UsageSplitAcrossEvents(t *testing.T) {
	t.Parallel()

	body := sseEvent("message_start",
		`{"type":"message_start","message":{"usage":{"input_tokens":42,"output_tokens":0}}}`) +
		sseEvent("content_block_delta",
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`) +
		sseEvent("message_delta",
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":99}}`) +
		sseEvent("message_stop", `{"type":"message_stop"}`)
	srv := newSSEServer(t, body)
	defer srv.Close()

	c := anthropic.New("sk-ant-test", anthropic.WithBaseURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mt := 64
	stream, err := c.ChatStream(ctx, provider.ChatRequest{
		Model:     "claude-opus-4-7",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: &mt,
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}
	chunks := collect(t, stream)
	terminal := chunks[len(chunks)-1]
	if terminal.Usage.InputTokens != 42 {
		t.Errorf("InputTokens = %d, want 42 (from message_start)", terminal.Usage.InputTokens)
	}
	if terminal.Usage.OutputTokens != 99 {
		t.Errorf("OutputTokens = %d, want 99 (from message_delta)", terminal.Usage.OutputTokens)
	}
}

func TestChatStream_PreStreamError_NoChannelAllocation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
	}))
	defer srv.Close()

	mt := 16
	c := anthropic.New("sk-ant-test", anthropic.WithBaseURL(srv.URL))
	stream, err := c.ChatStream(context.Background(), provider.ChatRequest{
		Model:     "claude-opus-4-7",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: &mt,
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
	c := anthropic.New("sk-ant-test")
	mt := 16
	_, err := c.ChatStream(context.Background(), provider.ChatRequest{
		Model:     "gpt-4o",
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

func TestChatStream_NilMaxTokens_InvalidInput(t *testing.T) {
	t.Parallel()
	c := anthropic.New("sk-ant-test")
	_, err := c.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "claude-opus-4-7",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		// MaxTokens nil — Anthropic requires it
	})
	var pe *provider.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not *ProviderError: %v", err)
	}
	if pe.Type != provider.ErrorTypeInvalidInput {
		t.Errorf("Type = %q, want %q", pe.Type, provider.ErrorTypeInvalidInput)
	}
}

// TestChatStream_MidStreamErrorEvent pins ADR-007 Q7 for the Anthropic
// "event: error" mid-stream frame — terminal chunk with Err set,
// channel close.
func TestChatStream_MidStreamErrorEvent(t *testing.T) {
	t.Parallel()

	body := sseEvent("message_start",
		`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0}}}`) +
		sseEvent("content_block_delta",
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`) +
		sseEvent("error",
			`{"type":"error","error":{"type":"overloaded_error","message":"too busy"}}`)
	srv := newSSEServer(t, body)
	defer srv.Close()

	c := anthropic.New("sk-ant-test", anthropic.WithBaseURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mt := 16
	stream, err := c.ChatStream(ctx, provider.ChatRequest{
		Model:     "claude-opus-4-7",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: &mt,
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}
	chunks := collect(t, stream)
	if len(chunks) < 2 {
		t.Fatalf("len(chunks) = %d, want >= 2 (partial + error terminal)", len(chunks))
	}
	terminal := chunks[len(chunks)-1]
	if terminal.Err == nil {
		t.Fatal("terminal Err nil — Q7 contract violation on event: error")
	}
	var pe *provider.ProviderError
	if !errors.As(terminal.Err, &pe) {
		t.Fatalf("terminal.Err is not *ProviderError: %v", terminal.Err)
	}
	if pe.Type != provider.ErrorTypeOverloaded {
		t.Errorf("Err.Type = %q, want %q (overloaded_error mapped)", pe.Type, provider.ErrorTypeOverloaded)
	}
	if !errors.Is(terminal.Err, provider.ErrOverloaded) {
		t.Errorf("errors.Is(err, ErrOverloaded) = false, want true")
	}
}

func TestChatStream_MalformedContentBlockDelta_ErrChunk(t *testing.T) {
	t.Parallel()

	body := sseEvent("message_start",
		`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0}}}`) +
		sseEvent("content_block_delta", "{not-json-at-all")
	srv := newSSEServer(t, body)
	defer srv.Close()

	c := anthropic.New("sk-ant-test", anthropic.WithBaseURL(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mt := 16
	stream, err := c.ChatStream(ctx, provider.ChatRequest{
		Model:     "claude-opus-4-7",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: &mt,
	})
	if err != nil {
		t.Fatalf("ChatStream err = %v", err)
	}
	chunks := collect(t, stream)
	terminal := chunks[len(chunks)-1]
	if terminal.Err == nil {
		t.Fatal("terminal Err nil — malformed content_block_delta should populate Err")
	}
	if terminal.FinishReason != provider.FinishUnknown {
		t.Errorf("terminal FinishReason = %q, want FinishUnknown", terminal.FinishReason)
	}
}

// TestChatStream_CtxCancel_NoErrChunk pins Q6 for both explicit cancel
// and deadline expiry — the close-only contract applies to both.
func TestChatStream_CtxCancel_NoErrChunk(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		mkCtx     func() (context.Context, context.CancelFunc)
		preCancel bool // explicit cancel after first chunk
	}{
		{
			name: "explicit_cancel",
			mkCtx: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			preCancel: true,
		},
		{
			name: "deadline_exceeded",
			mkCtx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 100*time.Millisecond)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			unblock := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)
				_, _ = w.Write([]byte(sseEvent("message_start",
					`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0}}}`)))
				_, _ = w.Write([]byte(sseEvent("content_block_delta",
					`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)))
				if flusher != nil {
					flusher.Flush()
				}
				<-unblock
			}))
			defer func() {
				close(unblock)
				srv.Close()
			}()

			c := anthropic.New("sk-ant-test", anthropic.WithBaseURL(srv.URL))
			ctx, cancel := tc.mkCtx()
			defer cancel()

			mt := 16
			stream, err := c.ChatStream(ctx, provider.ChatRequest{
				Model:     "claude-opus-4-7",
				Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
				MaxTokens: &mt,
			})
			if err != nil {
				t.Fatalf("ChatStream err = %v", err)
			}

			// Read one chunk
			first := <-stream
			if first.ContentDelta != "hi" {
				t.Errorf("first chunk content = %q", first.ContentDelta)
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
						t.Errorf("%s produced Err chunk = %v, want close only (Q6)", tc.name, chunk.Err)
					}
				case <-deadline:
					t.Fatal("channel did not close within 2s")
				}
			}
		})
	}
}

func TestChatStream_StopReasonMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want provider.FinishReason
	}{
		{"end_turn", provider.FinishStop},
		{"max_tokens", provider.FinishLength},
		{"stop_sequence", provider.FinishStopSequence},
		{"tool_use", provider.FinishToolUse},
		{"weird_new_value", provider.FinishUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			srv := newSSEServer(t, happyStreamBody([]string{"x"}, tc.raw))
			defer srv.Close()

			c := anthropic.New("sk-ant-test", anthropic.WithBaseURL(srv.URL))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			mt := 16
			stream, err := c.ChatStream(ctx, provider.ChatRequest{
				Model:     "claude-opus-4-7",
				Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
				MaxTokens: &mt,
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

func TestChatStream_SatisfiesStreamingProvider(t *testing.T) {
	t.Parallel()
	var p provider.Provider = anthropic.New("sk-ant-test")
	if _, ok := p.(provider.StreamingProvider); !ok {
		t.Fatal("anthropic.Client does not satisfy StreamingProvider")
	}
}
