package scenesim

import (
	"math/rand"
	"testing"
)

// F2 — the deeper-later residual. classifyLaneEntry is one-sided: it parks a store
// only for DEEPER active cross-origin sharers, never for a SHALLOWER one. So a
// deeper store that spawns AFTER a shallow store is already in flight (a retrieve
// freed the deep slot — the "non-store interference" the late-binding doc listed)
// admits immediately and races; if the shallow bin lands first it walls the deeper.
//
// This measures the residual rate and tests whether the owner's option (b) —
// symmetric Tier-2b, park the deeper while a shallower cross-origin store is active
// — rescues it. It does NOT: once the shallow bin is placed at the shallower slot,
// the deep slot BEHIND it is physically unreachable, so serializing the deeper
// after the shallower only guarantees the wall. This confirms the deeper-later case
// is fundamentally the banked LATE-BINDING residue (re-resolve the deep slot), not a
// dispatch-timing fix. Numbers are for the owner's decision.

// deeperLaterWalls runs one seed: a shallow store in flight, then a deeper store
// spawns. gated=true models Tier-2b (hold the deeper until the shallower completes);
// gated=false is the one-sided gate as built. Returns whether the deeper walled.
func deeperLaterWalls(t *testing.T, seed int, gated bool) bool {
	t.Helper()
	rng := rand.New(rand.NewSource(int64(seed)))
	sc := wideLaneScene(t, 4)
	sim := New(sc, Options{Watchdog: 80})
	sim.SetMouthGate(true)
	sim.SetPriorityOnly(true) // production physics
	sim.PlaceBin("S2")
	sim.PlaceBin("S3")

	// Shallow store → S0, dispatched first and already in flight.
	_ = sim.AddRobot("shallow", "AISLE")
	sim.SetRobotApproach("shallow", 1+rng.Intn(3))
	_ = sim.Submit("shallow", storeReq("shallow", "LINE", "S0"), false)

	if gated {
		// Tier-2b: hold the deeper until the shallower completes.
		for range 400 {
			sim.Tick()
			if sim.AllIdle() {
				break
			}
		}
	} else {
		// One-sided (as built): the deeper admits while the shallower is in flight.
		for range 1 + rng.Intn(3) {
			sim.Tick()
		}
	}

	// Deeper store → S1 (a retrieve just freed it), random distance.
	_ = sim.AddRobot("deep", "AISLE")
	sim.SetRobotApproach("deep", 1+rng.Intn(8))
	_ = sim.Submit("deep", storeReq("deep", "LINE", "S1"), false)

	_, _, settled := sim.RunUntilIdle(600)
	return sim.BusyCount() > 0 || !settled
}

func TestDeeperLater_Residual(t *testing.T) {
	const seeds = 200
	oneSided, gated := 0, 0
	for seed := range seeds {
		if deeperLaterWalls(t, seed, false) {
			oneSided++
		}
		if deeperLaterWalls(t, seed, true) {
			gated++
		}
	}
	t.Logf("deeper-later residual — ONE-SIDED gate (as built): %d/%d seeds wall (%.0f%%)",
		oneSided, seeds, float64(oneSided)*100/seeds)
	t.Logf("deeper-later residual — symmetric Tier-2b (park deeper until shallower done): %d/%d seeds wall (%.0f%%)",
		gated, seeds, float64(gated)*100/seeds)
	t.Logf("read: 2b does NOT rescue it — once the shallow bin is placed the deep slot behind it is unreachable; this is the banked late-binding case (re-resolve the deep slot). Owner decides accept-vs-latebinding.")
	if oneSided == 0 {
		t.Errorf("expected a measurable one-sided residual")
	}
}
