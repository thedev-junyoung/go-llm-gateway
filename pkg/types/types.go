// Package types holds vendor-neutral, leaf-level enums that every layer of the
// gateway agrees on. It exists to break a circular import: provider needs to
// reference Outcome / Origin (via AttemptInfo), and metrics needs them too —
// putting them in either of those packages would create a cycle once both
// participate in the observability path.
//
// Keep this package strictly leaf — no imports beyond stdlib. Any new shared
// vocabulary that crosses provider ↔ metrics ↔ gateway belongs here.
package types

// Outcome classifies the result of a single Chat attempt. The string values are
// stable wire labels (used as Prometheus label values, slog fields, etc.) so
// renaming a constant is a breaking change — pin the values in ADR-006 Q7.
type Outcome string

// Outcome values. success + 9 error_<type> variants matching ErrorType
// (see provider.OutcomeFromErr for the mapping).
const (
	OutcomeSuccess           Outcome = "success"
	OutcomeErrorRateLimit    Outcome = "error_rate_limit"
	OutcomeErrorAuth         Outcome = "error_auth"
	OutcomeErrorOverloaded   Outcome = "error_overloaded"
	OutcomeErrorServer       Outcome = "error_server"
	OutcomeErrorTimeout      Outcome = "error_timeout"
	OutcomeErrorInvalidInput Outcome = "error_invalid_input"
	OutcomeErrorNotFound     Outcome = "error_not_found"
	OutcomeErrorPermission   Outcome = "error_permission"
	OutcomeErrorUnknown      Outcome = "error_unknown"
)

// Origin distinguishes where an attempt was decided — vendor side, gateway-side
// rate-limit pre-empt, or gateway-side routing failure. Same wire-label
// stability contract as Outcome.
//
// The three states map directly to the operator hypothesis space ADR-006 Q3
// enumerates: gateway-preempt vs vendor "error_rate_limit" surface the same
// sentinel (ErrRateLimited) on the caller path, but they have different root
// causes and the metric label keeps them separable.
type Origin string

// Origin values. The three states ADR-006 Q3 pins.
const (
	OriginVendor         Origin = "vendor"          // provider responded (success or vendor-side error)
	OriginGatewayPreempt Origin = "gateway-preempt" // rate-limit pre-check blocked
	OriginGatewayRouter  Origin = "gateway-router"  // router failed to find a candidate
)
