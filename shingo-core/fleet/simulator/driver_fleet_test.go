//go:build sim

package simulator

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"shingocore/config"
)

// idx returns the position of want in xs, or -1.
func idx(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}

// G16: transit_min/_max make every move draw a uniform time in [min,max).
func TestDriverUniformTransitInRange(t *testing.T) {
	cfg := config.SimConfig{TransitMin: 10 * time.Minute, TransitMax: 20 * time.Minute}
	d, _, m, _ := newTestDriver(t, cfg, 42)
	now := m.Now()
	for i := 0; i < 2000; i++ {
		dur := d.nextDeadline(now).Sub(now)
		if dur < 10*time.Minute || dur >= 20*time.Minute {
			t.Fatalf("draw %d: transit %s out of [10m,20m)", i, dur)
		}
	}
}

// G16: with one robot, two orders created together must run one-at-a-time —
// the second can't go RUNNING until the first reaches FINISHED and frees the
// robot.
func TestDriverFiniteFleetSerializes(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 5 * time.Second, JitterPct: 0, FailRate: 0, FleetSize: 1}
	d, s, m, em := newTestDriver(t, cfg, 1)

	vid1 := mkTransport(t, s, "o1")
	vid2 := mkTransport(t, s, "o2")
	runTicks(d, m, 80)

	if s.GetOrder(vid1).State != "FINISHED" || s.GetOrder(vid2).State != "FINISHED" {
		t.Fatalf("both orders should finish; got %s, %s", s.GetOrder(vid1).State, s.GetOrder(vid2).State)
	}
	i1fin, i2run := idx(em.status, vid1+":FINISHED"), idx(em.status, vid2+":RUNNING")
	if i1fin < 0 || i2run < 0 {
		t.Fatalf("missing transitions: status=%v", em.status)
	}
	if i2run < i1fin {
		t.Fatalf("fleet_size=1 must serialize: order2 went RUNNING (idx %d) before order1 FINISHED (idx %d)", i2run, i1fin)
	}
	if got := d.Metrics(); got.MaxRobotsInUse != 1 || got.MaxQueued < 1 {
		t.Fatalf("metrics: want MaxRobotsInUse=1, MaxQueued>=1; got %+v", got)
	}
}

// Control: the infinite fleet (fleet_size unset) runs both orders concurrently —
// order2 goes RUNNING before order1 finishes. This is the legacy behaviour the
// finite-fleet path must not change when fleet_size is 0.
func TestDriverInfiniteFleetConcurrent(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 5 * time.Second, JitterPct: 0, FailRate: 0} // FleetSize 0
	d, s, m, em := newTestDriver(t, cfg, 1)

	vid1 := mkTransport(t, s, "o1")
	vid2 := mkTransport(t, s, "o2")
	runTicks(d, m, 80)

	i1fin, i2run := idx(em.status, vid1+":FINISHED"), idx(em.status, vid2+":RUNNING")
	if i1fin < 0 || i2run < 0 {
		t.Fatalf("missing transitions: status=%v", em.status)
	}
	if i2run > i1fin {
		t.Fatalf("infinite fleet should run concurrently: order2 RUNNING (idx %d) after order1 FINISHED (idx %d)", i2run, i1fin)
	}
	if got := d.Metrics(); got.FleetSize != 0 || got.RobotBusyTime != 0 || got.MaxQueued != 0 {
		t.Fatalf("infinite fleet must report empty fleet metrics; got %+v", got)
	}
}

// G16: utilization + queue metrics accrue. Four orders through a two-robot
// fleet: both robots saturate and at least two orders queue.
func TestDriverFleetMetrics(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 5 * time.Second, JitterPct: 0, FailRate: 0, FleetSize: 2}
	d, s, m, _ := newTestDriver(t, cfg, 7)

	for i := 0; i < 4; i++ {
		mkTransport(t, s, fmt.Sprintf("o%d", i))
	}
	runTicks(d, m, 60)

	got := d.Metrics()
	if got.FleetSize != 2 {
		t.Fatalf("FleetSize: want 2, got %d", got.FleetSize)
	}
	if got.Elapsed <= 0 || got.RobotBusyTime <= 0 {
		t.Fatalf("expected positive Elapsed and RobotBusyTime, got %+v", got)
	}
	if got.Utilization <= 0 || got.Utilization > 1.0 {
		t.Fatalf("Utilization out of (0,1]: %v (%+v)", got.Utilization, got)
	}
	if got.MaxRobotsInUse != 2 {
		t.Fatalf("MaxRobotsInUse: want 2 (saturated), got %d", got.MaxRobotsInUse)
	}
	if got.MaxQueued < 2 {
		t.Fatalf("MaxQueued: want >=2 (4 orders, 2 robots), got %d", got.MaxQueued)
	}
}

// G16: the finite-fleet path stays deterministic — same seed + config yields an
// identical transition sequence, including queueing order and uniform-transit
// draws (the property the sizing loops depend on for repeatable runs).
func TestDriverFiniteFleetDeterministic(t *testing.T) {
	cfg := config.SimConfig{
		TransitMin: 10 * time.Second, TransitMax: 30 * time.Second,
		FailRate: 0.1, FleetSize: 2,
	}
	run := func() []string {
		d, s, m, em := newTestDriver(t, cfg, 99)
		for i := 0; i < 6; i++ {
			mkTransport(t, s, fmt.Sprintf("o%d", i))
		}
		runTicks(d, m, 300)
		return em.status
	}
	a, b := run(), run()
	if len(a) == 0 {
		t.Fatal("expected some transitions")
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("non-deterministic finite-fleet sequence:\n a=%v\n b=%v", a, b)
	}
}
