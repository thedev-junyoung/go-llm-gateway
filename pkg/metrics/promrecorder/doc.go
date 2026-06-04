// Package promrecorder is the Prometheus implementation of
// metrics.MetricRecorder. It exposes three counters and one histogram
// covering the four operator-dashboard questions ADR-006 Q2 enumerates:
//
//   - "which vendor is failing right now?" — requests_total by outcome
//   - "how often does failover trigger?" — failovers_total
//   - "how does gateway pre-empt compare to vendor 429?" — requests_total
//     split by origin
//   - "which vendor is slow?" — attempt_duration_seconds p99
//
// Latency CONTRACT — OnAttempt / OnFailover are counter increments and one
// histogram Observe per call, all O(label-set) lookups. Safe to install
// synchronously on the gateway path. Wrap in metrics.AsyncWrapper only if
// the registered Registerer collects to a slow exporter (rare).
//
// Cardinality CONTRACT — the gateway normalizes Model via Config.KnownModels
// before calling OnAttempt, so the model label is capped at len(known)+1
// ("unknown"). PromRecorder itself does no normalization; it trusts the
// gateway to keep the input cardinality bounded. ADR-006 Q7.
package promrecorder
