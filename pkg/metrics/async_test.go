package metrics

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/thedev-junyoung/thedev-junyoung-go-llm-gateway/pkg/provider"
)

// syncFakeRecorder is the goroutine-safe variant used by async tests where
// the inner recorder runs on the consume goroutine and the test asserts from
// the main goroutine. Plain fakeRecorder is fine for sync tests but races here.
type syncFakeRecorder struct {
	mu        sync.Mutex
	attempts  []provider.AttemptInfo
	failovers []provider.FailoverInfo

	// block, if non-nil, makes OnAttempt wait until block is closed. Lets
	// tests pin the consume goroutine so the channel fills predictably.
	block chan struct{}

	// entered, if non-nil, is closed (once) the first time OnAttempt is
	// invoked. Lets tests synchronize on "consume has dequeued the first
	// event" before flooding the channel.
	entered   chan struct{}
	enterOnce sync.Once
}

func (f *syncFakeRecorder) OnAttempt(_ context.Context, info provider.AttemptInfo) {
	if f.entered != nil {
		f.enterOnce.Do(func() { close(f.entered) })
	}
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, info)
}

func (f *syncFakeRecorder) OnFailover(_ context.Context, info provider.FailoverInfo) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failovers = append(f.failovers, info)
}

func (f *syncFakeRecorder) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.attempts)
}

func (f *syncFakeRecorder) failoverCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.failovers)
}

// waitFor polls fn until it returns true or the deadline expires. Used to
// observe async side effects without a flaky fixed sleep.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waitFor: condition never satisfied within %s", timeout)
}

func TestAsync_ForwardsAttempts(t *testing.T) {
	inner := &syncFakeRecorder{}
	w := NewAsyncWrapper(inner, 16)
	defer func() { _ = w.Close() }()

	w.OnAttempt(context.Background(), sampleAttempt())

	waitFor(t, time.Second, func() bool { return inner.attemptCount() == 1 })
}

func TestAsync_ForwardsFailovers(t *testing.T) {
	inner := &syncFakeRecorder{}
	w := NewAsyncWrapper(inner, 16)
	defer func() { _ = w.Close() }()

	w.OnFailover(context.Background(), sampleFailover())

	waitFor(t, time.Second, func() bool { return inner.failoverCount() == 1 })
}

// TestAsync_DropsOnFullBuffer pins the drop-newest backpressure contract.
// Strategy: block the consume goroutine with a held inner.OnAttempt, then
// over-fill the channel and assert DroppedEvents matches the overflow count.
//
// Synchronization is tricky: the first send goes to consume, which then
// parks in inner. We must wait for consume to actually dequeue + enter
// inner before flooding — otherwise the first N sends are still in flight
// to consume when later sends arrive, and the drop count overcounts.
func TestAsync_DropsOnFullBuffer(t *testing.T) {
	block := make(chan struct{})
	entered := make(chan struct{})
	inner := &syncFakeRecorder{block: block, entered: entered}
	const bufSize = 4
	w := NewAsyncWrapper(inner, bufSize)
	defer func() {
		close(block)
		_ = w.Close()
	}()

	// Seed: this is the event consume will park on.
	w.OnAttempt(context.Background(), sampleAttempt())
	<-entered // consume has dequeued and entered inner — channel is empty

	// Now fill the channel exactly, then overflow.
	for i := 0; i < bufSize; i++ {
		w.OnAttempt(context.Background(), sampleAttempt())
	}
	const overflow = 5
	for i := 0; i < overflow; i++ {
		w.OnAttempt(context.Background(), sampleAttempt())
	}

	if got := w.DroppedEvents(); got != overflow {
		t.Errorf("DroppedEvents: want %d, got %d", overflow, got)
	}
}

// TestAsync_CloseIsIdempotent verifies the sync.Once guard — double-close
// is a common deferred-cleanup mistake and must not panic on a closed channel.
func TestAsync_CloseIsIdempotent(t *testing.T) {
	w := NewAsyncWrapper(&syncFakeRecorder{}, 4)
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestAsync_CloseStopsConsumeGoroutine catches a regression where Close fails
// to terminate consume — measured via runtime.NumGoroutine delta is flaky on
// shared test runs, so instead we close and then assert further sends still
// "succeed" (channel writable) but inner never sees them after the drain.
func TestAsync_CloseDrainsBufferedEvents(t *testing.T) {
	block := make(chan struct{})
	entered := make(chan struct{})
	inner := &syncFakeRecorder{block: block, entered: entered}
	w := NewAsyncWrapper(inner, 8)

	// Park consume on the first event so subsequent sends queue in the channel.
	w.OnAttempt(context.Background(), sampleAttempt())
	<-entered
	w.OnAttempt(context.Background(), sampleAttempt())
	w.OnAttempt(context.Background(), sampleAttempt())

	// Release consume so it can finish the first and pick up the queued ones.
	close(block)

	// Signal shutdown and let consume drain.
	_ = w.Close()

	waitFor(t, time.Second, func() bool { return inner.attemptCount() == 3 })
}
