package provider_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/types"
)

func TestOutcomeFromErr_NilIsSuccess(t *testing.T) {
	t.Parallel()
	if got := provider.OutcomeFromErr(nil); got != types.OutcomeSuccess {
		t.Errorf("OutcomeFromErr(nil) = %q, want %q", got, types.OutcomeSuccess)
	}
}

func TestOutcomeFromErr_AllErrorTypesMap(t *testing.T) {
	t.Parallel()

	// Exhaustive table — adding a new ErrorType MUST add an entry here, or
	// OutcomeFromErr silently downgrades it to "unknown" and the operator
	// dashboard loses a category. The test is the schema lock.
	cases := []struct {
		errType provider.ErrorType
		want    types.Outcome
	}{
		{provider.ErrorTypeRateLimit, types.OutcomeErrorRateLimit},
		{provider.ErrorTypeAuth, types.OutcomeErrorAuth},
		{provider.ErrorTypeOverloaded, types.OutcomeErrorOverloaded},
		{provider.ErrorTypeServer, types.OutcomeErrorServer},
		{provider.ErrorTypeTimeout, types.OutcomeErrorTimeout},
		{provider.ErrorTypeInvalidInput, types.OutcomeErrorInvalidInput},
		{provider.ErrorTypeNotFound, types.OutcomeErrorNotFound},
		{provider.ErrorTypePermission, types.OutcomeErrorPermission},
		{provider.ErrorTypeUnknown, types.OutcomeErrorUnknown},
	}

	for _, tc := range cases {
		t.Run(string(tc.errType), func(t *testing.T) {
			t.Parallel()
			pe := provider.NewProviderError("test", tc.errType, 0, false, "msg", nil)
			if got := provider.OutcomeFromErr(pe); got != tc.want {
				t.Errorf("OutcomeFromErr(%q) = %q, want %q", tc.errType, got, tc.want)
			}
		})
	}
}

func TestOutcomeFromErr_NonProviderErrorIsUnknown(t *testing.T) {
	t.Parallel()

	if got := provider.OutcomeFromErr(errors.New("plain")); got != types.OutcomeErrorUnknown {
		t.Errorf("plain error → %q, want %q", got, types.OutcomeErrorUnknown)
	}
}

func TestOutcomeFromErr_UnwrapsViaErrorsAs(t *testing.T) {
	t.Parallel()

	// Wrapping the ProviderError in another error MUST still map to the
	// underlying Outcome — failover loops in upstream code rely on
	// errors.As-based unwrap working through fmt.Errorf %w.
	pe := provider.NewProviderError("openai", provider.ErrorTypeRateLimit, 429, true, "throttled", nil)
	wrapped := fmt.Errorf("upstream: %w", pe)

	if got := provider.OutcomeFromErr(wrapped); got != types.OutcomeErrorRateLimit {
		t.Errorf("wrapped → %q, want %q", got, types.OutcomeErrorRateLimit)
	}
}
