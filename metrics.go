package gateway

import (
	"context"
	"errors"
	"log/slog"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/metrics"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// unknownModelLabel is the Outcome/Prometheus label used when req.Model is
// absent from Config.KnownModels. Pinned as a constant so dashboards and
// alert queries can match on it. See ADR-006 Q7.
const unknownModelLabel = "unknown"

// normalizeModel applies the KnownModels whitelist. Returns m unchanged when
// it's present in known; otherwise returns unknownModelLabel.
//
// IMPORTANT: an empty/nil known set normalizes EVERY model to "unknown"
// (strict default, per ADR-006 Q7). The rationale is cardinality protection
// plus an explicit alert signal — operators register their models on Config
// or accept that every dashboard label collapses to "unknown" until they do.
// This is intentionally hostile to silent rollouts of new vendor model ids.
//
// Both branches must keep the result stable for a given (m, known) — the
// recorder and ChatResponse.Attempts MUST see byte-identical model values
// so dashboards joining the two paths line up.
func normalizeModel(m string, known map[string]struct{}) string {
	if _, ok := known[m]; ok {
		return m
	}
	return unknownModelLabel
}

// recordAttempt invokes recorder.OnAttempt under recover(). The gateway
// installs MetricRecorder directly (callers can pass any implementation,
// including bare structs that have no panic guard of their own), so the
// gateway re-runs the same protective recover() that pkg/metrics.MultiRecorder
// applies internally. A panicking recorder must not crash Chat.
func recordAttempt(ctx context.Context, r metrics.MetricRecorder, info provider.AttemptInfo) {
	defer func() {
		if p := recover(); p != nil {
			slog.ErrorContext(ctx, "gateway metrics recorder OnAttempt panicked",
				"panic", p,
				"vendor", info.Vendor,
				"model", info.Model,
			)
		}
	}()
	r.OnAttempt(ctx, info)
}

// recordFailover is the failover-path twin of recordAttempt.
func recordFailover(ctx context.Context, r metrics.MetricRecorder, info provider.FailoverInfo) {
	defer func() {
		if p := recover(); p != nil {
			slog.ErrorContext(ctx, "gateway metrics recorder OnFailover panicked",
				"panic", p,
				"from_vendor", info.FromVendor,
				"to_vendor", info.ToVendor,
			)
		}
	}()
	r.OnFailover(ctx, info)
}

// asProviderError extracts the *ProviderError chain entry, or nil. Centralised
// so the Chat-loop call sites stay readable — every emission point needs the
// "if it's a ProviderError, capture it" pattern.
func asProviderError(err error) *provider.ProviderError {
	if err == nil {
		return nil
	}
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}
