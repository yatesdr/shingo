package engine

import (
	"sync/atomic"
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

// TestAfterQueue_StopDropsWhatIsStillQueued: at stop the piece running
// finishes and the pieces behind it are dropped, as run's comment says. The
// drain used to look at stop only between wakes, so once Engine.Stop waited
// for it a backlog held Core's shutdown for as long as the backlog took.
func TestAfterQueue_StopDropsWhatIsStillQueued(t *testing.T) {
	t.Parallel()
	q := newAfterQueue(t.Logf)
	q.turnover.Store(liveness.New(t.Logf).Loop("the after-emit queue"))
	stop, done := make(chan struct{}), make(chan struct{})
	started, release := make(chan struct{}), make(chan struct{})
	var ranQueued atomic.Bool
	q.later(func() { close(started); <-release })
	q.later(func() { ranQueued.Store(true) })
	go func() { q.run(stop); close(done) }()
	<-started
	close(stop)
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain did not return after stop")
	}
	if ranQueued.Load() {
		t.Fatal("work still queued at stop was run: stop waits for the whole backlog")
	}
}

// TestEngineStop_OneDeadlineOverEveryWait: the limit covers all of Stop's
// waits, not only the last. A wedged window pull (or a wedged scan pass) used
// to hold Core's shutdown with no limit, ahead of the one wait that had one.
func TestEngineStop_OneDeadlineOverEveryWait(t *testing.T) {
	t.Parallel()
	e := &Engine{stopChan: make(chan struct{}), logFn: func(string, ...any) {}}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	e.stage2.spawn(func() { <-release })
	done := make(chan struct{})
	go func() { e.stopBounded(100 * time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop was still waiting on a wedged window pull long after its limit")
	}
}
