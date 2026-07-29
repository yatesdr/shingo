//go:build sim

package simulator

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"shingocore/config"
)

// The driver used to stamp "sim-bot-"+vendorOrderID on every RUNNING
// transition, so the sim's "fleet" had exactly as many members as the run had
// orders and every fleet-shaped query in store/telemetry — per-robot
// breakdown, robot filter, utilization, hourly concurrency — read a population
// of one. These pin the named pool that replaced it.

// robotsAssigned returns the distinct robot IDs the emitter saw, in first-seen
// order.
func robotsAssigned(em *captureEmitter) []string {
	em.mu.Lock()
	defer em.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, a := range em.assigned {
		id := a[strings.LastIndex(a, ":")+1:]
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// The brief's acceptance test: a run of many orders over a small fleet
// produces a handful of robots, not one per order.
func TestDriverPool_FleetSizeBoundsDistinctRobots(t *testing.T) {
	const fleet = 7
	cfg := config.SimConfig{TransitTime: 2 * time.Second, JitterPct: 0, FailRate: 0, FleetSize: fleet}
	d, s, m, em := newTestDriver(t, cfg, 1)

	var vids []string
	for i := range 40 {
		vids = append(vids, mkTransport(t, s, fmt.Sprintf("o%d", i)))
	}
	// Well short of defaultRetention (10m) so nothing is evicted mid-assertion.
	runTicks(d, m, 300)

	for _, vid := range vids {
		if !contains(em.status, vid+":FINISHED") {
			t.Fatalf("order %s did not finish: %v", vid, em.status)
		}
	}

	robots := robotsAssigned(em)
	if len(robots) == 0 || len(robots) > fleet {
		t.Fatalf("40 orders over a fleet of %d should use at most %d distinct robots, got %d: %v",
			fleet, fleet, len(robots), robots)
	}
	// The whole point: far fewer robots than orders.
	if len(robots) >= len(vids) {
		t.Fatalf("robot count (%d) must be well under order count (%d)", len(robots), len(vids))
	}
	for _, id := range robots {
		if !strings.HasPrefix(id, "AMR-") {
			t.Fatalf("robot id %q is not a plant-style vehicle id", id)
		}
	}
}

// FIFO reuse, so a lightly loaded sim rotates the pool instead of pinning
// AMR-01. Without this a per-robot breakdown has one busy row and six idle.
func TestDriverPool_RotatesRatherThanPinning(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 2 * time.Second, JitterPct: 0, FailRate: 0, FleetSize: 4}
	d, s, m, em := newTestDriver(t, cfg, 1)

	// One order at a time: each finishes before the next is created, so a LIFO
	// pool would hand out AMR-01 every time.
	for i := range 4 {
		vid := mkTransport(t, s, fmt.Sprintf("solo%d", i))
		runTicks(d, m, 60)
		if got := s.GetOrder(vid).State; got != "FINISHED" {
			t.Fatalf("order %d did not finish: %s", i, got)
		}
	}

	if robots := robotsAssigned(em); len(robots) < 2 {
		t.Fatalf("serial orders should rotate through the pool, got %v", robots)
	}
}

// Identity is exclusive: no two orders hold the same robot at the same time.
// Checked against the driver's own bookkeeping rather than the emit log,
// because that is where a double-assignment would actually originate.
func TestDriverPool_NoConcurrentDuplicates(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 3 * time.Second, JitterPct: 0, FailRate: 0, FleetSize: 5}
	d, s, m, _ := newTestDriver(t, cfg, 7)

	for i := range 12 {
		mkTransport(t, s, fmt.Sprintf("x%d", i))
	}

	for range 400 {
		m.Advance(time.Second)
		d.step(m.Now())

		held := map[string]string{}
		for vid, p := range d.progress {
			if p.robotID == "" {
				continue
			}
			if other, dup := held[p.robotID]; dup {
				t.Fatalf("robot %s held by both %s and %s", p.robotID, other, vid)
			}
			held[p.robotID] = vid
		}
		if len(held) > cfg.FleetSize {
			t.Fatalf("%d robots in use exceeds fleet size %d", len(held), cfg.FleetSize)
		}
	}
}

// The infinite fleet (fleet_size unset) keeps unbounded capacity but still
// hands out exclusive, reusable names — the pool settles at peak concurrency
// instead of growing with the order count.
func TestDriverPool_InfiniteFleetStillReusesNames(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 2 * time.Second, JitterPct: 0, FailRate: 0} // FleetSize 0
	d, s, m, em := newTestDriver(t, cfg, 3)

	// Serial orders: capacity is unbounded but only one is ever live, so the
	// pool should never need a second name.
	for i := range 6 {
		vid := mkTransport(t, s, fmt.Sprintf("inf%d", i))
		runTicks(d, m, 60)
		if got := s.GetOrder(vid).State; got != "FINISHED" {
			t.Fatalf("order %d did not finish: %s", i, got)
		}
	}

	if robots := robotsAssigned(em); len(robots) != 1 {
		t.Fatalf("serial orders on an unbounded fleet should reuse one name, got %v", robots)
	}
	if d.mintedBots != 1 {
		t.Fatalf("pool should have minted exactly one robot, minted %d", d.mintedBots)
	}
}

// A robot must come back to the pool when the driver abandons an order, not
// just when it finishes — otherwise a fault leaks a pool slot and a finite
// fleet bleeds down to zero over a long soak.
func TestDriverPool_FaultReturnsTheRobot(t *testing.T) {
	cfg := config.SimConfig{TransitTime: 2 * time.Second, JitterPct: 0, FailRate: 1.0, FleetSize: 3}
	d, s, m, _ := newTestDriver(t, cfg, 5)

	for i := range 6 {
		mkTransport(t, s, fmt.Sprintf("f%d", i))
	}
	runTicks(d, m, 200)

	if d.robotsInUse != 0 {
		t.Fatalf("every order faulted; robotsInUse should be 0, got %d", d.robotsInUse)
	}
	if len(d.freeRobots) != cfg.FleetSize {
		t.Fatalf("pool should be whole again: want %d free, got %d (%v)",
			cfg.FleetSize, len(d.freeRobots), d.freeRobots)
	}
}
