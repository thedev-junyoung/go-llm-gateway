// Package logrecorder is the slog-backed implementation of
// metrics.MetricRecorder. It emits one structured log record per Chat
// attempt and per failover handoff using the field vocabulary ADR-006 Q5
// pins (request_id, vendor, model, attempt, outcome, duration_ms, origin).
//
// Compose with promrecorder when you want both metrics and logs — pass
// `metrics.Multi(promrecorder.New(), logrecorder.NewDefault())` as
// Config.Metrics. The MultiRecorder iterates with per-recorder recover()
// so a slog handler panic does not skip the prometheus emit.
//
// Latency CONTRACT — slog records are essentially marshal + io.Write.
// stdlib JSONHandler / TextHandler are safe to install synchronously on
// the gateway path. If you wire a slow handler (network sink, S3 batcher)
// you MUST wrap in metrics.AsyncWrapper — see pkg/metrics doc CONTRACT.
package logrecorder
