// Package metrics defines the gateway's MetricRecorder interface and the
// wrapper utilities (NoOpRecorder, MultiRecorder, AsyncWrapper) that compose
// recorder backends without changing the gateway's call signature.
//
// The interface follows ADR-005's pluggable-backend pattern: callers either
// supply a Prometheus implementation (pkg/metrics/promrecorder) or write
// their own adapter (OTel, Datadog, log-only, etc.). The gateway passes
// every Chat attempt and failover handoff through the recorder with the
// observability primitives defined in pkg/types and pkg/provider.
//
// CONTRACT — read this before writing a recorder:
//
//   - OnAttempt / OnFailover MUST return immediately. The gateway invokes
//     them on the caller's goroutine, so a blocking implementation directly
//     widens every Chat latency. Fast backends (Prometheus Counter is atomic,
//     NoOp is a no-op) are fine to register synchronously. Slow backends
//     (OTel remote exporter, Datadog flush) MUST be wrapped in AsyncWrapper.
//
//   - Recorders SHOULD NOT panic. The gateway already protects against
//     panics via recover() (so a buggy recorder doesn't crash Chat), but a
//     recorder that panics on its own goroutine — anything internally
//     spawned via `go ...` — is outside recover's reach and will crash the
//     process.
//
// See ADR-006 for the full design (Q1 interface choice, Q5 logging
// separation, Q6 OTel deferral, AsyncWrapper Sub-decision).
package metrics
