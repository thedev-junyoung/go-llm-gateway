package logrecorder_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/internal/requestctx"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/metrics/logrecorder"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/types"
)

// captureHandler is a slog.Handler that records every emitted record
// instead of writing anywhere. Lets tests assert on level, msg, and the
// full attribute set without parsing JSON / text output.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}

// attrMap collapses a record's attributes into a name→value map for easier
// assertion than walking the attribute iterator.
func attrMap(r slog.Record) map[string]any {
	m := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	return m
}

func newRecorderWithCapture(t *testing.T) (*logrecorder.LogRecorder, *captureHandler) {
	t.Helper()
	h := &captureHandler{}
	return logrecorder.New(slog.New(h)), h
}

func TestOnAttempt_Success_EmitsInfoWithAllFields(t *testing.T) {
	t.Parallel()

	r, h := newRecorderWithCapture(t)
	ctx := requestctx.With(context.Background(), "req-abc")

	r.OnAttempt(ctx, provider.AttemptInfo{
		Vendor:   "openai",
		Model:    "gpt-4o",
		AttemptN: 0,
		Outcome:  types.OutcomeSuccess,
		Origin:   types.OriginVendor,
		Duration: 250 * time.Millisecond,
	})

	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Level != slog.LevelInfo {
		t.Errorf("Level = %v, want Info on success", rec.Level)
	}
	if rec.Message != logrecorder.MsgAttempt {
		t.Errorf("Message = %q, want %q", rec.Message, logrecorder.MsgAttempt)
	}

	attrs := attrMap(rec)
	want := map[string]any{
		"request_id":  "req-abc",
		"vendor":      "openai",
		"model":       "gpt-4o",
		"attempt":     int64(0),
		"outcome":     "success",
		"duration_ms": int64(250),
		"origin":      "vendor",
	}
	for k, v := range want {
		if attrs[k] != v {
			t.Errorf("attr %q = %v, want %v", k, attrs[k], v)
		}
	}
}

func TestOnAttempt_Error_EmitsWarnWithErrorFields(t *testing.T) {
	t.Parallel()

	r, h := newRecorderWithCapture(t)
	pe := provider.NewProviderError("openai", provider.ErrorTypeRateLimit, 429, true, "slow down", nil)

	r.OnAttempt(context.Background(), provider.AttemptInfo{
		Vendor:  "openai",
		Model:   "gpt-4o",
		Outcome: types.OutcomeErrorRateLimit,
		Origin:  types.OriginVendor,
		Error:   pe,
	})

	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Level != slog.LevelWarn {
		t.Errorf("Level = %v, want Warn on error outcome", rec.Level)
	}

	attrs := attrMap(rec)
	if attrs["error_type"] != "rate_limit" {
		t.Errorf("error_type = %v, want %q", attrs["error_type"], "rate_limit")
	}
	if attrs["error_message"] != "slow down" {
		t.Errorf("error_message = %v, want %q", attrs["error_message"], "slow down")
	}
}

func TestOnAttempt_ErrorWithoutProviderError_NoErrorFields(t *testing.T) {
	t.Parallel()

	r, h := newRecorderWithCapture(t)

	// Outcome is non-success but Error is nil (router-failure surface where
	// the original error wasn't a *ProviderError, or a bare error path).
	// The recorder must NOT panic dereferencing nil.
	r.OnAttempt(context.Background(), provider.AttemptInfo{
		Vendor:  "gateway",
		Model:   "unknown",
		Outcome: types.OutcomeErrorUnknown,
		Origin:  types.OriginGatewayRouter,
	})

	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(recs))
	}
	attrs := attrMap(recs[0])
	if _, present := attrs["error_type"]; present {
		t.Errorf("error_type should be absent when Error is nil")
	}
	if _, present := attrs["error_message"]; present {
		t.Errorf("error_message should be absent when Error is nil")
	}
}

func TestOnFailover_EmitsInfoWithFailoverFields(t *testing.T) {
	t.Parallel()

	r, h := newRecorderWithCapture(t)
	ctx := requestctx.With(context.Background(), "req-xyz")

	r.OnFailover(ctx, provider.FailoverInfo{
		FromVendor: "openai",
		ToVendor:   "anthropic",
		Reason:     provider.ErrorTypeOverloaded,
	})

	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Level != slog.LevelInfo {
		t.Errorf("Level = %v, want Info (failover is procedural)", rec.Level)
	}
	if rec.Message != logrecorder.MsgFailover {
		t.Errorf("Message = %q, want %q", rec.Message, logrecorder.MsgFailover)
	}

	attrs := attrMap(rec)
	want := map[string]any{
		"request_id":  "req-xyz",
		"from_vendor": "openai",
		"to_vendor":   "anthropic",
		"reason":      "error_overloaded",
	}
	for k, v := range want {
		if attrs[k] != v {
			t.Errorf("attr %q = %v, want %v", k, attrs[k], v)
		}
	}
}

// TestOnAttempt_NoRequestID_EmptyString pins the contract that a missing
// request_id surfaces as an empty string rather than absent attribute —
// log-aggregator filter rules like `request_id IS NULL` need a consistent
// key shape across all records.
func TestOnAttempt_NoRequestID_EmptyString(t *testing.T) {
	t.Parallel()

	r, h := newRecorderWithCapture(t)
	r.OnAttempt(context.Background(), provider.AttemptInfo{
		Vendor:  "openai",
		Model:   "gpt-4o",
		Outcome: types.OutcomeSuccess,
		Origin:  types.OriginVendor,
	})

	attrs := attrMap(h.snapshot()[0])
	if attrs["request_id"] != "" {
		t.Errorf("request_id = %v, want empty string when ctx has none", attrs["request_id"])
	}
}

// TestOnFailover_NoRequestID_EmptyString mirrors the attempt-side guard.
// OnFailover also pulls request_id from ctx; missing it must surface the
// same empty-string shape so the two record types stay filter-symmetric
// in log aggregators.
func TestOnFailover_NoRequestID_EmptyString(t *testing.T) {
	t.Parallel()

	r, h := newRecorderWithCapture(t)
	r.OnFailover(context.Background(), provider.FailoverInfo{
		FromVendor: "openai",
		ToVendor:   "anthropic",
		Reason:     provider.ErrorTypeRateLimit,
	})

	attrs := attrMap(h.snapshot()[0])
	if attrs["request_id"] != "" {
		t.Errorf("request_id = %v, want empty string when ctx has none", attrs["request_id"])
	}
}

// TestNew_NilLogger_Panics keeps the constructor contract explicit. A nil
// logger would NPE on the first OnAttempt call; failing fast at New is
// easier to diagnose than a panic mid-Chat.
func TestNew_NilLogger_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("New(nil) did not panic")
		}
	}()
	_ = logrecorder.New(nil)
}

func TestNewDefault_UsesGlobalSlog(t *testing.T) {
	t.Parallel()
	// Smoke test — NewDefault must not panic and must return a usable recorder.
	r := logrecorder.NewDefault()
	r.OnAttempt(context.Background(), provider.AttemptInfo{
		Vendor:  "openai",
		Model:   "gpt-4o",
		Outcome: types.OutcomeSuccess,
		Origin:  types.OriginVendor,
	})
}
