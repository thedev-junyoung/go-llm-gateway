package metrics

import "time"

// Streaming outcome label values for the TTFT and stream_duration
// histograms. Pinned as exported constants so PromRecorder dashboards,
// LogRecorder queries, and gateway emission sites share a single
// vocabulary. See ADR-007 Q5.
const (
	// StreamOutcomeSuccess is observed when the first ContentDelta!=""
	// chunk reaches the caller (for TTFT) or when the stream completes
	// without an Err chunk (for stream_duration).
	StreamOutcomeSuccess = "success"

	// StreamOutcomePreStreamFailure is observed when the underlying
	// adapter's ChatStream returned a *ProviderError before any chunk
	// channel was opened. The gateway's ChatStream loop emits this
	// from the failover-retry branch.
	StreamOutcomePreStreamFailure = "pre_stream_failure"

	// StreamOutcomeCtxCancelBeforeFirstChunk is observed when the
	// caller's ctx is Done before any ContentDelta!="" chunk arrives.
	// Distinct from PreStreamFailure because the failure cause is
	// caller-side, not vendor-side — operators alert differently.
	StreamOutcomeCtxCancelBeforeFirstChunk = "ctx_cancel_before_first_chunk"

	// StreamOutcomeMidStreamError is observed when the stream emits a
	// terminal chunk with Err != nil (ADR-007 Q7). For stream_duration
	// only — TTFT already observed Success if a chunk arrived first.
	StreamOutcomeMidStreamError = "mid_stream_error"
)

// StreamingMetricRecorder extends MetricRecorder with the two
// histograms ADR-007 Q5 introduces for streaming observability.
// Recorders that don't care about streaming metrics (NoOpRecorder,
// LogRecorder in v0.2) can satisfy this with empty implementations;
// the gateway's wrapWithMetrics path type-asserts and skips the
// Observe calls when the configured recorder doesn't satisfy it.
//
// Same pattern as provider.StreamingProvider — separate interface,
// type assertion at the consumer, no breaking change to MetricRecorder.
type StreamingMetricRecorder interface {
	MetricRecorder

	// ObserveFirstTokenLatency records the time from gateway.ChatStream
	// entry to the first ContentDelta!="" chunk (or to the
	// pre-stream-failure / ctx-cancel terminal point — distinguished
	// via the outcome label).
	ObserveFirstTokenLatency(vendor, model, outcome string, d time.Duration)

	// ObserveStreamDuration records the full lifecycle from
	// gateway.ChatStream entry until the producer goroutine closes the
	// channel. Outcome distinguishes normal completion from pre-stream
	// failure, ctx cancel, and mid-stream error.
	ObserveStreamDuration(vendor, model, outcome string, d time.Duration)
}
