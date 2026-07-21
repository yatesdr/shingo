package scenesim

import (
	"fmt"
	"math/rand"
	"testing"
)

// robotsInLane counts robots physically inside a lane right now.
func robotsInLane(sim *Sim, lane string) int {
	n := 0
	for _, id := range sim.order {
		r := sim.robots[id]
		if r.pos.inLane() && r.pos.Lane == lane {
			n++
		}
	}
	return n
}

// TestModeShare_PressPairCoOccupy: two same-kind (inbound) stores legally
// co-occupy one single-file lane — leader deep, follower shallow, no pass-through
// — so BOTH are physically inside at once, and both complete with no violation.
// This is the store-behind-store / press-pair sharing capacity-1 could not model.
func TestModeShare_PressPairCoOccupy(t *testing.T) {
	sc := plantScene(t) // 3-slot lane A1: S0 S1 S2
	sim := New(sc, Options{Watchdog: 400})
	sim.SetMouthGate(true)
	_ = sim.AddRobot("LEFT", "AISLE")
	_ = sim.AddRobot("RIGHT", "AISLE")

	_ = sim.Submit("LEFT", storeReq("left-1", "LINE-IN", "A1-S2"), false)   // deep leader
	_ = sim.Submit("RIGHT", storeReq("right-1", "LINE-IN", "A1-S1"), false) // shallow follower

	maxInLane := 0
	var vios []Violation
	for range 500 {
		vios = append(vios, sim.Tick()...)
		if n := robotsInLane(sim, "LANE-A1"); n > maxInLane {
			maxInLane = n
		}
		if sim.AllIdle() {
			break
		}
	}
	if !sim.AllIdle() {
		t.Fatal("same-kind pair did not settle")
	}
	if maxInLane < 2 {
		t.Fatalf("same-kind pair never co-occupied the lane (max robots in lane = %d) — sharing not exercised", maxInLane)
	}
	for _, v := range vios {
		t.Errorf("checker fired on a legal same-kind co-occupancy: %s: %s", v.Checker, v.Detail)
	}
}

// runSoakSeed drives one deterministic random store stream over a slots-wide
// lane under the mouth gate (share = mode-share vs capacity-1) and returns the
// ticks to drain; it flags non-settle or any violation via violCount.
func runSoakSeed(t *testing.T, seed, slots int, share bool, violCount *int) int {
	t.Helper()
	rng := rand.New(rand.NewSource(int64(seed)))
	sc := wideLaneScene(t, slots)
	sim := New(sc, Options{Watchdog: 120})
	sim.SetMouthGate(true)
	sim.SetLaneCapacity1(!share)

	n := 1 + rng.Intn(slots)
	targets := rng.Perm(slots)[:n]
	for i, slotIdx := range targets {
		rid := fmt.Sprintf("R%d", i)
		if err := sim.AddRobot(rid, "AISLE"); err != nil {
			t.Fatalf("seed %d AddRobot: %v", seed, err)
		}
		req := storeReq(fmt.Sprintf("o%d-%d", seed, i), "LINE", fmt.Sprintf("S%d", slotIdx))
		if err := sim.Submit(rid, req, false); err != nil {
			t.Fatalf("seed %d Submit: %v", seed, err)
		}
	}
	ticks, vios, settled := sim.RunUntilIdle(4000)
	if !settled || len(vios) > 0 {
		*violCount++
	}
	return ticks
}

// TestModeShare_ThroughputVsCapacity1: over the same 200 random store streams,
// BOTH admission modes stay clean, and mode-share drains no slower than capacity-1
// (usually faster, from same-kind co-occupancy). This is the menu-pinning
// evidence — what sharing buys over the conservative one-at-a-time baseline.
func TestModeShare_ThroughputVsCapacity1(t *testing.T) {
	const seeds = 200
	const slots = 4
	var shareTicks, capTicks, shareViol, capViol int
	for seed := range seeds {
		shareTicks += runSoakSeed(t, seed, slots, true, &shareViol)
		capTicks += runSoakSeed(t, seed, slots, false, &capViol)
	}
	if shareViol > 0 || capViol > 0 {
		t.Fatalf("soak violations: mode-share=%d capacity-1=%d seeds (both must be clean)", shareViol, capViol)
	}
	pct := 100 * float64(shareTicks) / float64(capTicks)
	t.Logf("throughput over %d seeds (%d-slot lane): mode-share=%d ticks vs capacity-1=%d ticks (%.1f%% of baseline)",
		seeds, slots, shareTicks, capTicks, pct)
	if shareTicks > capTicks {
		t.Errorf("mode-share (%d ticks) drained SLOWER than capacity-1 (%d ticks) — sharing must never cost throughput",
			shareTicks, capTicks)
	}
}

// TestModeShare_MixedKindExcluded: while a dig works the lane, a store cannot
// co-occupy (different kind) — it is kept out until the dig clears, and the lane
// never holds mixed work.
func TestModeShare_MixedKindExcluded(t *testing.T) {
	sc := plantScene(t)
	sim := New(sc, Options{Watchdog: 40})
	sim.SetMouthGate(true)
	_ = sim.AddRobot("DIG", "AISLE")
	_ = sim.AddRobot("STORE", "AISLE")

	_ = sim.Submit("DIG", carryStoreWait("dig-1", "LINE-IN", "A1-S2"), true) // dig holds the lane
	vio := ticksCollect(sim, 100)
	_ = sim.Submit("STORE", storeReq("store-1", "LINE-IN", "A1-S1"), false)
	vio = append(vio, ticksCollect(sim, 100)...)

	if robotsInLane(sim, "LANE-A1") > 1 {
		t.Fatal("store co-occupied the dig's lane — mixed kind must be excluded")
	}
	if hasChecker(vio, "mode-purity") {
		t.Fatalf("mode purity violated while the dig held the lane: %+v", vio)
	}

	sim.ReleaseWait("dig-1")
	_, more, settled := sim.RunUntilIdle(500)
	vio = append(vio, more...)
	if !settled {
		t.Fatal("did not settle after the dig released")
	}
	for _, v := range vio {
		t.Errorf("checker fired under the mouth gate: %s: %s", v.Checker, v.Detail)
	}
}
