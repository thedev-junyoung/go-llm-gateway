package logrecorder

import (
	"context"
	"log/slog"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/internal/requestctx"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/metrics"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/types"
)

// Log message constants. Exported so callers can reference them
// symbolically in test assertions, log-router rules, and alert queries —
// renaming either is a breaking change for every existing search /
// dashboard / saved filter, so pin them as a public contract.
const (
	MsgAttempt  = "gateway attempt"
	MsgFailover = "gateway failover"
)

// LogRecorder implements metrics.MetricRecorder by emitting one slog
// record per OnAttempt / OnFailover using the field vocabulary pinned in
// ADR-006 Q5: request_id, vendor, model, attempt, outcome, duration_ms,
// origin. Errors carry an extra error_type + error_message pair.
type LogRecorder struct {
	logger *slog.Logger
}

// Compile-time assertion that LogRecorder satisfies MetricRecorder.
var _ metrics.MetricRecorder = (*LogRecorder)(nil)

// New returns a LogRecorder that emits to the given logger. The logger
// must be non-nil — passing nil is almost certainly a wiring bug and
// would NPE on the first OnAttempt, so we surface it at construction.
func New(logger *slog.Logger) *LogRecorder {
	if logger == nil {
		panic("logrecorder: New called with nil logger; use NewDefault() to install slog.Default()")
	}
	return &LogRecorder{logger: logger}
}

// NewDefault returns a LogRecorder that captures slog.Default() at
// construction time — calling slog.SetDefault after construction will
// NOT redirect this recorder's output. Use this when the global slog
// handler is already configured and will not be swapped at runtime.
// For dynamic handler swaps, construct your own *slog.Logger and pass
// it to New so you control the lifecycle.
func NewDefault() *LogRecorder {
	return &LogRecorder{logger: slog.Default()}
}

// OnAttempt emits one structured log record per attempt. Severity is
// outcome-driven: Info on success, Warn on every error variant. Operators
// who want a different severity policy (e.g. Error on auth failures) can
// install a handler that re-rates levels by outcome.
func (r *LogRecorder) OnAttempt(ctx context.Context, info provider.AttemptInfo) {
	attrs := []slog.Attr{
		slog.String("request_id", requestctx.From(ctx)),
		slog.String("vendor", info.Vendor),
		slog.String("model", info.Model),
		slog.Int("attempt", info.AttemptN),
		slog.String("outcome", string(info.Outcome)),
		slog.Int64("duration_ms", info.Duration.Milliseconds()),
		slog.String("origin", string(info.Origin)),
	}
	if info.Error != nil {
		attrs = append(attrs,
			slog.String("error_type", string(info.Error.Type)),
			slog.String("error_message", info.Error.VendorMessage()),
		)
	}

	level := slog.LevelInfo
	if info.Outcome != types.OutcomeSuccess {
		level = slog.LevelWarn
	}
	r.logger.LogAttrs(ctx, level, MsgAttempt, attrs...)
}

// OnFailover emits one Info-level record per router handoff. Failover is
// procedural (the gateway recovering from a retriable failure) so Warn
// would over-flag it — the underlying attempt's Warn record already
// carries the failure detail.
func (r *LogRecorder) OnFailover(ctx context.Context, info provider.FailoverInfo) {
	r.logger.LogAttrs(ctx, slog.LevelInfo, MsgFailover,
		slog.String("request_id", requestctx.From(ctx)),
		slog.String("from_vendor", info.FromVendor),
		slog.String("to_vendor", info.ToVendor),
		slog.String("reason", "error_"+string(info.Reason)),
	)
}
