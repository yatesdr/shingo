package fulfillment

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol/clock"
)

// TestPeriodicSweep_FollowsTheSimClock pins that the scanner's periodic sweep is
// paced by protocol/clock, not by the wall: under a 10x sim clock a sweep of
// interval d fires every d/10 of wall time, and under 1x every d. On a plant the
// default clock is the real one, so nothing there changes.
//
// It matters to every sim pair: a wait served only by the sweep would read ten
// times longer in the sim than on a plant if the ticker ran on the wall. The
// sweep takes clock.Default() when its goroutine starts, and the sim backend
// installs its clock (cmd/shingocore/sim_enabled.go) before the engine starts it.
//
// Not parallel: it swaps the process default clock and restores it on return,
// before any parallel test in this package runs.
func TestPeriodicSweep_FollowsTheSimClock(t *testing.T) {
	const interval = time.Second // simulated
	const window = 2 * time.Second
	for _, tc := range []struct {
		speed    float64
		min, max int32 // sweeps expected in the wall window
	}{
		{speed: 10, min: 15, max: 21}, // every 100 ms of wall: 20 in 2 s
		{speed: 1, min: 1, max: 3},    // every 1 s of wall: 2 in 2 s
	} {
		prev := clock.Default()
		clock.SetDefault(clock.NewRunningClock(tc.speed))

		f := newFakeStore()
		var sweeps int32
		f.onListQueuedOrders = func() { atomic.AddInt32(&sweeps, 1) }
		s := newTestScanner(t, f)
		s.StartPeriodicSweep(interval)
		time.Sleep(window)
		s.Stop()
		clock.SetDefault(prev)

		if got := atomic.LoadInt32(&sweeps); got < tc.min || got > tc.max {
			t.Errorf("at %.0fx a %s sweep fired %d times in %s of wall, want %d..%d — the ticker is not "+
				"following the sim clock", tc.speed, interval, got, window, tc.min, tc.max)
		}
	}
}

// TestPeriodicSweep_StopWaitsForAPassInFlight: Stop returns only once a sweep
// pass it had started has finished. It used to return at once, and the pass
// went on against a database the engine's caller was about to close, logging
// through a logger whose owner could be gone (the class behind CI run
// 43042514760; Engine.Stop calls this).
func TestPeriodicSweep_StopWaitsForAPassInFlight(t *testing.T) {
	f := newFakeStore()
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var once sync.Once
	var finished atomic.Bool
	f.onListQueuedOrders = func() {
		once.Do(func() {
			close(entered)
			<-release
			finished.Store(true)
		})
	}
	s := newTestScanner(t, f)
	s.StartPeriodicSweep(10 * time.Millisecond)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep did not start a pass in 5 s")
	}

	stopped := make(chan struct{})
	go func() {
		s.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a sweep pass it had started was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the pass finished")
	}
	if !finished.Load() {
		t.Fatal("Stop returned before the pass finished")
	}
}
