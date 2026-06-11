//go:build sim

package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"shingo/shared/clock"
)

func newTestSimOperator(clk clock.Clock) *simOperator {
	return &simOperator{
		e:       &Engine{logFn: func(string, ...any) {}, debugFn: func(string, ...any) {}},
		clk:     clk,
		ctx:     context.Background(),
		pending: make(map[int64]bool),
	}
}

// T3.2 / Gate 3: a delivered event fires the operator action after the delay,
// and a duplicate delivery to the same node while the first is pending does not
// double-fire.
func TestSimOperator_FiresOnceAfterDelay(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	op := newTestSimOperator(m)
	var calls atomic.Int32
	op.classify = func(int64) (time.Duration, string, func() error, bool) {
		return 5 * time.Second, "load", func() error { calls.Add(1); return nil }, true
	}

	op.schedule(42)
	op.schedule(42) // duplicate while pending — must be dropped (dedup is synchronous)

	// Drive the manual clock until the single worker's delay elapses. Advancing
	// before the worker registers its waiter is harmless; loop until it fires.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		m.Advance(5 * time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // a wrongly-spawned dup worker would fire here

	if got := calls.Load(); got != 1 {
		t.Fatalf("want exactly 1 action (deduped, after delay), got %d", got)
	}
	op.mu.Lock()
	stillPending := op.pending[42]
	op.mu.Unlock()
	if stillPending {
		t.Fatal("pending[42] should be cleared after the action ran")
	}
}

// T3.2: a node that doesn't classify as a loader/unloader produces no action.
func TestSimOperator_NoActionWhenNotClassified(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	op := newTestSimOperator(m)
	var calls atomic.Int32
	op.classify = func(int64) (time.Duration, string, func() error, bool) {
		return 0, "", nil, false
	}
	op.schedule(7)
	m.Advance(10 * time.Second)
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("want 0 actions for an unclassified node, got %d", calls.Load())
	}
}

// T3.2: a delivery with no ProcessNodeID is ignored (no panic, no schedule).
func TestSimOperator_IgnoresNilProcessNode(t *testing.T) {
	op := newTestSimOperator(clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	op.classify = func(int64) (time.Duration, string, func() error, bool) {
		t.Fatal("classify must not run for a nil-ProcessNodeID delivery")
		return 0, "", nil, false
	}
	op.onDelivered(Event{Type: EventOrderDelivered, Payload: OrderDeliveredEvent{OrderID: 1}})
	time.Sleep(20 * time.Millisecond)
}
