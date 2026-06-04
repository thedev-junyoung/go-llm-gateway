package promrecorder_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/metrics/promrecorder"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/types"
)

// newIsolated builds a PromRecorder against a fresh Registry so concurrent
// tests don't collide on the prometheus default registerer.
func newIsolated(t *testing.T) (*promrecorder.PromRecorder, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	return promrecorder.NewWithRegisterer(reg), reg
}

func TestOnAttempt_VendorSuccess_IncrementsRequestsAndObservesDuration(t *testing.T) {
	t.Parallel()

	r, reg := newIsolated(t)
	r.OnAttempt(context.Background(), provider.AttemptInfo{
		Vendor:   "openai",
		Model:    "gpt-4o",
		Outcome:  types.OutcomeSuccess,
		Origin:   types.OriginVendor,
		Duration: 250 * time.Millisecond,
	})

	// Pin the full label set with GatherAndCompare so a label rename or
	// reorder fails loudly — CollectAndCount only catches series-count drift.
	const wantRequests = `
# HELP llm_gateway_requests_total Total number of Chat attempts the gateway observed, partitioned by vendor, model, outcome, and origin (vendor / gateway-preempt / gateway-router).
# TYPE llm_gateway_requests_total counter
llm_gateway_requests_total{model="gpt-4o",origin="vendor",outcome="success",vendor="openai"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantRequests), "llm_gateway_requests_total"); err != nil {
		t.Errorf("requests_total exposition mismatch: %v", err)
	}

	// Histogram series count is the cleanest "did we observe?" check —
	// GatherAndCompare on the histogram would have to enumerate every
	// bucket boundary which is brittle to the bucket-list constant.
	if got := testutil.CollectAndCount(reg, "llm_gateway_attempt_duration_seconds"); got == 0 {
		t.Errorf("attempt_duration_seconds was not observed for vendor-origin attempt")
	}
}

// TestOnAttempt_PreemptOrigin_SkipsHistogram pins the documented behavior
// that pre-empt/router origins do NOT contribute to the duration histogram.
// They carry Duration=0 (the vendor was never reached) and would corrupt
// p50/p95/p99 buckets. See ADR-006 synthesis pseudocode.
func TestOnAttempt_PreemptOrigin_SkipsHistogram(t *testing.T) {
	t.Parallel()

	r, reg := newIsolated(t)
	r.OnAttempt(context.Background(), provider.AttemptInfo{
		Vendor:  "openai",
		Model:   "gpt-4o",
		Outcome: types.OutcomeErrorRateLimit,
		Origin:  types.OriginGatewayPreempt,
		// Duration intentionally zero
	})

	if got := testutil.CollectAndCount(reg, "llm_gateway_requests_total"); got != 1 {
		t.Errorf("requests_total = %d, want 1 (pre-empt still counts as an attempt)", got)
	}
	if got := testutil.CollectAndCount(reg, "llm_gateway_attempt_duration_seconds"); got != 0 {
		t.Errorf("attempt_duration_seconds = %d, want 0 (pre-empt MUST NOT pollute the histogram)", got)
	}
}

func TestOnAttempt_RouterOrigin_SkipsHistogram(t *testing.T) {
	t.Parallel()

	r, reg := newIsolated(t)
	r.OnAttempt(context.Background(), provider.AttemptInfo{
		Vendor:  "gateway",
		Model:   "unknown",
		Outcome: types.OutcomeErrorInvalidInput,
		Origin:  types.OriginGatewayRouter,
	})

	if got := testutil.CollectAndCount(reg, "llm_gateway_attempt_duration_seconds"); got != 0 {
		t.Errorf("attempt_duration_seconds = %d, want 0 (router-failure MUST NOT pollute the histogram)", got)
	}
}

// TestOnAttempt_UnknownModel_IncrementsUnknownTotal pins the alert signal:
// a spike in unknown_model_total means a new vendor model id leaked through
// without operator-side KnownModels registration. PromRecorder MUST emit
// this whenever the gateway's normalization produced "unknown".
func TestOnAttempt_UnknownModel_IncrementsUnknownTotal(t *testing.T) {
	t.Parallel()

	r, reg := newIsolated(t)
	r.OnAttempt(context.Background(), provider.AttemptInfo{
		Vendor:  "openai",
		Model:   "unknown",
		Outcome: types.OutcomeSuccess,
		Origin:  types.OriginVendor,
	})

	const want = `
# HELP llm_gateway_unknown_model_total Number of Chat attempts whose Model label collapsed to "unknown" because the gateway's Config.KnownModels did not include the requested model id. A spike means a new vendor model leaked through without configuration.
# TYPE llm_gateway_unknown_model_total counter
llm_gateway_unknown_model_total{vendor="openai"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "llm_gateway_unknown_model_total"); err != nil {
		t.Errorf("unknown_model_total exposition mismatch: %v", err)
	}
}

func TestOnAttempt_KnownModel_DoesNotIncrementUnknownTotal(t *testing.T) {
	t.Parallel()

	r, reg := newIsolated(t)
	r.OnAttempt(context.Background(), provider.AttemptInfo{
		Vendor:  "openai",
		Model:   "gpt-4o",
		Outcome: types.OutcomeSuccess,
		Origin:  types.OriginVendor,
	})

	if got := testutil.CollectAndCount(reg, "llm_gateway_unknown_model_total"); got != 0 {
		t.Errorf("unknown_model_total series = %d, want 0 (known model MUST NOT increment)", got)
	}
}

// TestOnFailover_ReasonLabelFormat pins the "error_<type>" normalization
// so failovers_total.reason cross-queries cleanly against
// requests_total.outcome — both use the same wire vocabulary.
func TestOnFailover_ReasonLabelFormat(t *testing.T) {
	t.Parallel()

	r, reg := newIsolated(t)
	r.OnFailover(context.Background(), provider.FailoverInfo{
		FromVendor: "openai",
		ToVendor:   "anthropic",
		Reason:     provider.ErrorTypeRateLimit,
	})

	// One series exists.
	if got := testutil.CollectAndCount(reg, "llm_gateway_failovers_total"); got != 1 {
		t.Errorf("failovers_total series = %d, want 1", got)
	}
	// And its labels carry the normalized reason. testutil.GatherAndCompare
	// is the canonical assertion for full label fidelity.
	const expectedExposition = `
# HELP llm_gateway_failovers_total Total number of failover handoffs between consecutive Chat attempts. The reason label is normalized as error_<type> to align with requests_total.outcome for cross-querying.
# TYPE llm_gateway_failovers_total counter
llm_gateway_failovers_total{from_vendor="openai",reason="error_rate_limit",to_vendor="anthropic"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expectedExposition), "llm_gateway_failovers_total"); err != nil {
		t.Errorf("failovers_total label mismatch: %v", err)
	}
}

// TestNewWithRegisterer_RegistersAllFourMetrics is the smoke test that
// catches accidental drops of any metric vector during refactors —
// gathering the registry after a minimal probe must expose all four families.
// We emit one OnAttempt (Model=unknown + Origin=vendor → hits requests +
// unknown_model + duration histogram) and one OnFailover (→ failovers) to
// force every vector to materialize at least one series. Prometheus omits
// silent metric families from Gather output, so a probe-free Gather would
// always return zero series even when registration succeeded.
func TestNewWithRegisterer_RegistersAllFourMetrics(t *testing.T) {
	t.Parallel()

	r, reg := newIsolated(t)
	r.OnAttempt(context.Background(), provider.AttemptInfo{
		Vendor:   "openai",
		Model:    "unknown",
		Outcome:  types.OutcomeSuccess,
		Origin:   types.OriginVendor,
		Duration: 100 * time.Millisecond,
	})
	r.OnFailover(context.Background(), provider.FailoverInfo{
		FromVendor: "openai",
		ToVendor:   "anthropic",
		Reason:     provider.ErrorTypeRateLimit,
	})

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather err = %v", err)
	}
	want := map[string]bool{
		"llm_gateway_requests_total":           false,
		"llm_gateway_failovers_total":          false,
		"llm_gateway_unknown_model_total":      false,
		"llm_gateway_attempt_duration_seconds": false,
	}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, present := range want {
		if !present {
			t.Errorf("metric %q not registered", name)
		}
	}
}
