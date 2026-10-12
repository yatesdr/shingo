package engine

import (
	"testing"
	"time"

	"shingo/protocol/liveness"
)

// The after-queue's depth counts work handed over and not yet finished, and
// its drain runs each piece as one turn of the freeze alarm's loop.
func TestAfterQueue_DepthCountsUnfinishedWorkAndTheDrainIsWatched(t *testing.T) {
	t.Parallel()
	q := newAfterQueue(t.Logf)
	watch := liveness.New(t.Logf)
	q.turnover.Store(watch.Loop("the after-emit queue"))
	stop := make(chan struct{})
	defer close(stop)
	go q.run(stop)

	release := make(chan struct{})
	started := make(chan struct{})
	q.later(func() { close(started); <-release })
	q.later(func() {})
	<-started
	if d := q.depth.Load(); d != 2 {
		t.Errorf("depth with one piece running and one queued = %d, want 2", d)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for q.depth.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if d := q.depth.Load(); d != 0 {
		t.Errorf("depth after the drain = %d, want 0", d)
	}
	if f := watch.Frozen(); len(f) != 0 {
		t.Errorf("a drained queue reads frozen: %v", f)
	}
}

// A panicking piece is recovered, leaves the turn and still counts down.
func TestAfterQueue_APanicLeavesTheTurnAndTheDepth(t *testing.T) {
	t.Parallel()
	q := newAfterQueue(func(string, ...any) {})
	stop := make(chan struct{})
	defer close(stop)
	go q.run(stop)

	done := make(chan struct{})
	q.later(func() { panic("boom") })
	q.later(func() { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the piece after a panic never ran")
	}
	deadline := time.Now().Add(2 * time.Second)
	for q.depth.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if d := q.depth.Load(); d != 0 {
		t.Errorf("depth after a panic and a drain = %d, want 0", d)
	}
}
