package promrecorder

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/metrics"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/types"
)

// Metric names. Pinned as constants so dashboard queries and alert rules
// have a single source of truth — renaming a constant is a breaking change
// for every existing operator setup.
const (
	metricRequestsTotal        = "llm_gateway_requests_total"
	metricFailoversTotal       = "llm_gateway_failovers_total"
	metricUnknownModelTotal    = "llm_gateway_unknown_model_total"
	metricAttemptDurationSecs  = "llm_gateway_attempt_duration_seconds"
	metricFirstTokenLatencySec = "llm_gateway_first_token_latency_seconds"
	metricStreamDurationSecs   = "llm_gateway_stream_duration_seconds"
)

// defaultDurationBuckets covers the typical LLM completion latency range
// (≈ 100ms for tiny prompts up to 60s for long generations). Prometheus's
// DefBuckets only reaches 10s which truncates a meaningful chunk of the
// real-world distribution for LLM workloads.
var defaultDurationBuckets = []float64{
	0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60,
}

// firstTokenBuckets is the TTFT (time-to-first-token) distribution.
// The user-perceived UX bar is sub-500ms; the buckets cluster more
// finely there than defaultDurationBuckets so dashboards see real
// movement when TTFT regresses by ±50ms (which IS user-visible).
var firstTokenBuckets = []float64{
	0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20,
}

// PromRecorder is the Prometheus-backed MetricRecorder. Construct with New
// (registers to prometheus.DefaultRegisterer) or NewWithRegisterer (custom
// Registerer — required for test isolation since the default Registerer
// rejects duplicate registrations).
type PromRecorder struct {
	requestsTotal     *prometheus.CounterVec
	failoversTotal    *prometheus.CounterVec
	unknownModelTotal *prometheus.CounterVec
	attemptDuration   *prometheus.HistogramVec
	firstTokenLatency *prometheus.HistogramVec
	streamDuration    *prometheus.HistogramVec
}

// Compile-time assertions: PromRecorder satisfies both the base and
// streaming-aware MetricRecorder interfaces. ADR-007 Q5.
var (
	_ metrics.MetricRecorder          = (*PromRecorder)(nil)
	_ metrics.StreamingMetricRecorder = (*PromRecorder)(nil)
)

// New returns a PromRecorder registered against prometheus.DefaultRegisterer.
// Panics on duplicate registration — the default Registerer rejects collisions
// to surface configuration mistakes early. Use NewWithRegisterer when you
// need a custom registry (multi-tenant setups, tests).
func New() *PromRecorder {
	return NewWithRegisterer(prometheus.DefaultRegisterer)
}

// NewWithRegisterer registers the metric vectors against reg and returns
// the recorder. reg must be non-nil — a nil Registerer would panic at
// MustRegister anyway; we catch it explicitly so the error message points
// at the wiring mistake instead of a deep stack into client_golang.
func NewWithRegisterer(reg prometheus.Registerer) *PromRecorder {
	if reg == nil {
		panic("promrecorder: NewWithRegisterer called with nil Registerer")
	}
	r := &PromRecorder{
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricRequestsTotal,
			Help: "Total number of Chat attempts the gateway observed, partitioned by vendor, model, outcome, and origin (vendor / gateway-preempt / gateway-router).",
		}, []string{"vendor", "model", "outcome", "origin"}),

		failoversTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricFailoversTotal,
			Help: "Total number of failover handoffs between consecutive Chat attempts. The reason label is normalized as error_<type> to align with requests_total.outcome for cross-querying.",
		}, []string{"from_vendor", "to_vendor", "reason"}),

		unknownModelTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricUnknownModelTotal,
			Help: "Number of gateway attempts (Chat or ChatStream) whose Model label collapsed to \"unknown\" because the gateway's Config.KnownModels did not include the requested model id. A spike means a new vendor model leaked through without configuration.",
		}, []string{"vendor"}),

		attemptDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    metricAttemptDurationSecs,
			Help:    "Distribution of per-attempt latency in seconds. Observed only when origin == vendor — pre-empt and router-failure attempts have Duration=0 and would corrupt the p50/p95/p99 buckets.",
			Buckets: defaultDurationBuckets,
		}, []string{"vendor", "model", "outcome"}),

		firstTokenLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    metricFirstTokenLatencySec,
			Help:    "Distribution of streaming time-to-first-token (TTFT) in seconds. Observed once per ChatStream call. The outcome label distinguishes the success case (first ContentDelta!=\"\" arrived) from pre-stream failure and ctx-cancel-before-first-chunk paths.",
			Buckets: firstTokenBuckets,
		}, []string{"vendor", "model", "outcome"}),

		streamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    metricStreamDurationSecs,
			Help:    "Distribution of streaming lifecycle duration in seconds — from ChatStream entry until the producer goroutine closes the channel. Outcome distinguishes normal completion (success) from pre-stream failure, ctx cancel, and mid-stream error.",
			Buckets: defaultDurationBuckets,
		}, []string{"vendor", "model", "outcome"}),
	}
	reg.MustRegister(r.requestsTotal, r.failoversTotal, r.unknownModelTotal,
		r.attemptDuration, r.firstTokenLatency, r.streamDuration)
	return r
}

// OnAttempt records one attempt. Increments requests_total unconditionally;
// increments unknown_model_total only when the gateway's normalization
// produced "unknown"; observes attempt_duration_seconds only for vendor-
// origin attempts (preempt/router carry Duration=0 and would skew the
// histogram). See ADR-006 synthesis pseudocode.
func (r *PromRecorder) OnAttempt(_ context.Context, info provider.AttemptInfo) {
	r.requestsTotal.WithLabelValues(
		info.Vendor,
		info.Model,
		string(info.Outcome),
		string(info.Origin),
	).Inc()

	if info.Model == "unknown" {
		r.unknownModelTotal.WithLabelValues(info.Vendor).Inc()
	}

	if info.Origin == types.OriginVendor {
		r.attemptDuration.WithLabelValues(
			info.Vendor,
			info.Model,
			string(info.Outcome),
		).Observe(info.Duration.Seconds())
	}
}

// OnFailover records one router handoff. The reason label is the outgoing
// attempt's ErrorType wrapped as "error_<type>" so dashboards can cross-
// query against requests_total.outcome without label-format mismatch.
func (r *PromRecorder) OnFailover(_ context.Context, info provider.FailoverInfo) {
	r.failoversTotal.WithLabelValues(
		info.FromVendor,
		info.ToVendor,
		"error_"+string(info.Reason),
	).Inc()
}

// ObserveFirstTokenLatency records one TTFT observation. The gateway's
// wrapWithMetrics emits exactly one of these per ChatStream call. The
// outcome label is one of metrics.StreamOutcome* constants — using the
// shared vocabulary keeps PromQL queries portable across recorders.
func (r *PromRecorder) ObserveFirstTokenLatency(vendor, model, outcome string, d time.Duration) {
	r.firstTokenLatency.WithLabelValues(vendor, model, outcome).Observe(d.Seconds())

	// Reuse the existing unknown_model alert path so the same operator
	// query covers sync + streaming. Without this, a Loki-only operator
	// staring at the streaming surface would miss the unknown-model
	// drift signal.
	if model == "unknown" {
		r.unknownModelTotal.WithLabelValues(vendor).Inc()
	}
}

// ObserveStreamDuration records one full-stream-lifecycle observation.
// One per ChatStream call. Same outcome vocabulary as
// ObserveFirstTokenLatency, with the addition of
// StreamOutcomeMidStreamError for streams that emitted a terminal
// Err chunk after producing some content.
func (r *PromRecorder) ObserveStreamDuration(vendor, model, outcome string, d time.Duration) {
	r.streamDuration.WithLabelValues(vendor, model, outcome).Observe(d.Seconds())
}
