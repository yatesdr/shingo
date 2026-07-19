package scenesim

import "testing"

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

	_ = sim.Submit("LEFT", storeReq("left-1", "LINE-IN", "A1-S2"), false)  // deep leader
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
