package provider

import (
	"errors"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/types"
)

// AttemptInfo captures one vendor attempt's outcome. The gateway accumulates
// these in ChatResponse.Attempts and feeds the same value into
// MetricRecorder.OnAttempt — see ADR-006 Q4.
//
// Lives in pkg/provider (not pkg/metrics or pkg/types) because it composes
// provider-domain values (Usage, *ProviderError) directly. The cross-cutting
// label enums (Outcome, Origin) live in pkg/types so metrics and provider can
// reference them without cycling. See ADR-006 Q4 sub-decision.
type AttemptInfo struct {
	// Vendor is Provider.Name() for vendor-side attempts, or "gateway" when
	// the router failed before any provider was reached (origin=gateway-router).
	Vendor string

	// Model is the gateway-normalized model id — already passed through
	// Config.KnownModels whitelist, so "unknown" if the caller's id wasn't
	// registered. See ADR-006 Q7.
	Model string

	// AttemptN is 0 for the primary, 1+ for fallback candidates in slice order.
	AttemptN int

	Outcome  types.Outcome
	Origin   types.Origin
	Duration time.Duration

	// Usage is populated on success. Error is populated on failure. Both may
	// be zero-value when origin == OriginGatewayRouter (no provider reached).
	Usage Usage
	Error *ProviderError
}

// FailoverInfo describes one router-level handoff — emitted between two
// consecutive attempts when the prior one returned a retriable
// *ProviderError. Reason is the *outgoing* attempt's ErrorType so dashboards
// can attribute failovers to their trigger.
type FailoverInfo struct {
	FromVendor string
	ToVendor   string
	Reason     ErrorType
}

// OutcomeFromErr maps an arbitrary error into the closed Outcome enum.
// errors.As unwraps; non-*ProviderError and unmatched ErrorType both
// degrade to OutcomeErrorUnknown so callers always get a finite label set.
//
// nil → OutcomeSuccess so adapters can call OutcomeFromErr(err) uniformly
// in their success path.
func OutcomeFromErr(err error) types.Outcome {
	if err == nil {
		return types.OutcomeSuccess
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		return types.OutcomeErrorUnknown
	}
	switch pe.Type {
	case ErrorTypeRateLimit:
		return types.OutcomeErrorRateLimit
	case ErrorTypeAuth:
		return types.OutcomeErrorAuth
	case ErrorTypeOverloaded:
		return types.OutcomeErrorOverloaded
	case ErrorTypeServer:
		return types.OutcomeErrorServer
	case ErrorTypeTimeout:
		return types.OutcomeErrorTimeout
	case ErrorTypeInvalidInput:
		return types.OutcomeErrorInvalidInput
	case ErrorTypeNotFound:
		return types.OutcomeErrorNotFound
	case ErrorTypePermission:
		return types.OutcomeErrorPermission
	default:
		return types.OutcomeErrorUnknown
	}
}
