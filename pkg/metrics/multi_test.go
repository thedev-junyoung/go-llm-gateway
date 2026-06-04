package metrics

import (
	"context"
	"testing"
)

func TestMulti_FansOutToAllRecorders(t *testing.T) {
	a, b, c := &fakeRecorder{}, &fakeRecorder{}, &fakeRecorder{}
	m := Multi(a, b, c)

	m.OnAttempt(context.Background(), sampleAttempt())
	m.OnFailover(context.Background(), sampleFailover())

	for i, r := range []*fakeRecorder{a, b, c} {
		if len(r.attempts) != 1 {
			t.Errorf("recorder %d: want 1 attempt, got %d", i, len(r.attempts))
		}
		if len(r.failovers) != 1 {
			t.Errorf("recorder %d: want 1 failover, got %d", i, len(r.failovers))
		}
	}
}

// TestMulti_PanicInOneDoesNotSkipOthers is the core safety property the
// MultiRecorder docs claim — one bad recorder must not silently swallow
// events destined for the rest. Ordering matters: put the panicker first
// so we'd notice if the loop short-circuits.
func TestMulti_PanicInOneDoesNotSkipOthers(t *testing.T) {
	bad := &fakeRecorder{panicOnNext: true}
	good := &fakeRecorder{}
	m := Multi(bad, good)

	m.OnAttempt(context.Background(), sampleAttempt())

	if len(good.attempts) != 1 {
		t.Fatalf("downstream recorder skipped after upstream panic: got %d attempts", len(good.attempts))
	}
}

func TestMulti_PanicInFailoverDoesNotSkipOthers(t *testing.T) {
	bad := &fakeRecorder{panicOnNext: true}
	good := &fakeRecorder{}
	m := Multi(bad, good)

	m.OnFailover(context.Background(), sampleFailover())

	if len(good.failovers) != 1 {
		t.Fatalf("downstream recorder skipped after upstream panic: got %d failovers", len(good.failovers))
	}
}

// TestMulti_EmptyIsNoOp keeps the zero-len case explicit so a future refactor
// that swaps the slice for something panicking on empty input fails loudly.
func TestMulti_EmptyIsNoOp(_ *testing.T) {
	m := Multi()
	m.OnAttempt(context.Background(), sampleAttempt())
	m.OnFailover(context.Background(), sampleFailover())
}
