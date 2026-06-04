package types_test

import (
	"testing"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/types"
)

// TestOutcomeWireLabels pins the string values of Outcome enums.
// These values are emitted as Prometheus label values + slog fields, so any
// rename is a breaking change for downstream dashboards / alert rules / log
// queries. Fail loud if anyone edits a constant without realizing the impact.
func TestOutcomeWireLabels(t *testing.T) {
	t.Parallel()

	cases := map[types.Outcome]string{
		types.OutcomeSuccess:           "success",
		types.OutcomeErrorRateLimit:    "error_rate_limit",
		types.OutcomeErrorAuth:         "error_auth",
		types.OutcomeErrorOverloaded:   "error_overloaded",
		types.OutcomeErrorServer:       "error_server",
		types.OutcomeErrorTimeout:      "error_timeout",
		types.OutcomeErrorInvalidInput: "error_invalid_input",
		types.OutcomeErrorNotFound:     "error_not_found",
		types.OutcomeErrorPermission:   "error_permission",
		types.OutcomeErrorUnknown:      "error_unknown",
	}

	for got, want := range cases {
		if string(got) != want {
			t.Errorf("Outcome wire label drift: %q != %q", got, want)
		}
	}
}

func TestOriginWireLabels(t *testing.T) {
	t.Parallel()

	cases := map[types.Origin]string{
		types.OriginVendor:         "vendor",
		types.OriginGatewayPreempt: "gateway-preempt",
		types.OriginGatewayRouter:  "gateway-router",
	}

	for got, want := range cases {
		if string(got) != want {
			t.Errorf("Origin wire label drift: %q != %q", got, want)
		}
	}
}
