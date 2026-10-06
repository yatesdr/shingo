package fulfillment

import (
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
