package metrics

import (
	"context"
	"testing"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/types"
)

// TestNoOpRecorder_DoesNotPanic exercises both hook signatures with zero-value
// inputs. The point isn't behavior (there is none) — it's that NoOpRecorder
// stays safe to install as the default even when the gateway sends empty
// AttemptInfo on early router failures.
func TestNoOpRecorder_DoesNotPanic(_ *testing.T) {
	var r NoOpRecorder
	r.OnAttempt(context.Background(), provider.AttemptInfo{})
	r.OnFailover(context.Background(), provider.FailoverInfo{})
}

// TestNoOpRecorder_NilContext guards against accidental ctx.Value lookups
// in a future refactor — the recorder must tolerate a nil ctx the same way
// it tolerates a real one, since the gateway docs don't promise non-nil ctx.
func TestNoOpRecorder_NilContext(_ *testing.T) {
	var r NoOpRecorder
	//nolint:staticcheck // SA1012 — intentional: pin the nil-tolerance contract.
	r.OnAttempt(nil, provider.AttemptInfo{Vendor: "openai"})
	//nolint:staticcheck // SA1012 — intentional: pin the nil-tolerance contract.
	r.OnFailover(nil, provider.FailoverInfo{FromVendor: "openai", ToVendor: "anthropic"})
}

// fakeRecorder is the test double used across multi_test and async_test.
// Counts calls and captures the last payload so tests can assert both fan-out
// volume and value-faithfulness. Not goroutine-safe by design — async_test
// uses a mutex-wrapped variant when concurrency matters.
type fakeRecorder struct {
	attempts    []provider.AttemptInfo
	failovers   []provider.FailoverInfo
	panicOnNext bool
}

func (f *fakeRecorder) OnAttempt(_ context.Context, info provider.AttemptInfo) {
	if f.panicOnNext {
		f.panicOnNext = false
		panic("fakeRecorder: induced panic")
	}
	f.attempts = append(f.attempts, info)
}

func (f *fakeRecorder) OnFailover(_ context.Context, info provider.FailoverInfo) {
	if f.panicOnNext {
		f.panicOnNext = false
		panic("fakeRecorder: induced panic")
	}
	f.failovers = append(f.failovers, info)
}

// sampleAttempt is the canonical AttemptInfo used across tests — pre-filled
// so individual cases can focus on the dimension under test.
func sampleAttempt() provider.AttemptInfo {
	return provider.AttemptInfo{
		Vendor:  "openai",
		Model:   "gpt-4o",
		Outcome: types.OutcomeSuccess,
		Origin:  types.OriginVendor,
	}
}

func sampleFailover() provider.FailoverInfo {
	return provider.FailoverInfo{
		FromVendor: "openai",
		ToVendor:   "anthropic",
		Reason:     provider.ErrorTypeRateLimit,
	}
}
