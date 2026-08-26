package scenesim

import (
	"fmt"
	"math/rand"
	"testing"

	"shingocore/fleet"
)

// Harness for the UNIFORM gated shape (increment 3).
//
// Production ships EVERY lane-bound store on a gated group as an
// unsealed waybill ending at the lane's wait point, and appends the dropoff tail
// when the lane is safe — immediately, back to back with the create, when it
// already is. There is no bypass class, so the open and contended cases differ
// only in WHEN the tail is appended, never in the shape of what was created.
//
// These scenarios model exactly that, using the real deferred tail (Submit the
// unsealed pair, then AppendBlocks) rather than a pre-known plan behind a flag —
// so what is under test is the design, not a mock of it.

// gatedStoreReq is what the valve CREATES: [pickup@source, wait@gate], unsealed.
// The dropoff does not exist yet — that is the whole point.
func gatedStoreReq(id, source, gate string) fleet.CreateOrderRequest {
	return fleet.CreateOrderRequest{
		OrderID: id,
		Blocks: []fleet.OrderBlock{
			{BlockID: id + "-b1", Location: source, BinTask: "JackLoad"},
			{BlockID: id + "-b2", Location: gate, BinTask: "Wait"},
		},
		Complete: false,
	}
}

// gatedTail is what the valve APPENDS to seal the order: [dropoff@slot]. Block
// numbering continues from the create (b3), as stepsToBlocks' blockOffset does.
func gatedTail(id, slot string) []fleet.OrderBlock {
	return []fleet.OrderBlock{{BlockID: id + "-b3", Location: slot, BinTask: "JackUnload"}}
}

// newGatedSim builds the standard lane scene under production physics (mode mouth
// gate + priority hint, no harness deepest-first hold) — the same arm the tiered
// tests measure against, so tick counts are comparable.
func newGatedSim(t *testing.T, slots, hopTicks int) *Sim {
	t.Helper()
	sim := New(wideLaneScene(t, slots), Options{Watchdog: 200, HopTicks: hopTicks})
	sim.SetMouthGate(true)
	sim.SetPriorityOnly(true)
	return sim
}

// runPressPair settles a same-origin pair either through the sealed pre-change
// shape or through the valve (create unsealed, append the tail immediately, back
// to back, before any tick — exactly what the scanner pass does when the
// classifier admits). Returns the settle tick count. Fails the test if any robot
// ever dwells in the gated run: an open valve must never park a robot, and that is
// checked EVERY tick so a single-tick dwell cannot hide between samples.
func runPressPair(t *testing.T, hopTicks int, gated bool) int {
	t.Helper()
	specs := []storeSpec{
		{id: "PRESS-L", slot: "S1", approach: 2}, // deep
		{id: "PRESS-R", slot: "S0", approach: 2}, // shallow
	}
	sim := newGatedSim(t, 4, hopTicks)
	for _, sp := range specs {
		if err := sim.AddRobot(sp.id, "AISLE"); err != nil {
			t.Fatalf("AddRobot %s: %v", sp.id, err)
		}
		sim.SetRobotApproach(sp.id, sp.approach)
		if gated {
			if err := sim.Submit(sp.id, gatedStoreReq(sp.id, "LINE", "GATE"), false); err != nil {
				t.Fatalf("Submit %s: %v", sp.id, err)
			}
			if err := sim.AppendBlocks(sp.id, gatedTail(sp.id, sp.slot)); err != nil {
				t.Fatalf("AppendBlocks %s: %v", sp.id, err)
			}
			continue
		}
		if err := sim.Submit(sp.id, storeReq(sp.id, "LINE", sp.slot), false); err != nil {
			t.Fatalf("Submit %s: %v", sp.id, err)
		}
	}
	for range 400 {
		for _, v := range sim.Tick() {
			t.Errorf("press pair (gated=%v) fired a checker: %s: %s", gated, v.Checker, v.Detail)
		}
		if gated {
			if w := sim.WaitingOrders(); len(w) > 0 {
				t.Fatalf("open valve must never dwell; robots waiting at tick %d: %v", sim.TickCount(), w)
			}
		}
		if sim.AllIdle() {
			return sim.TickCount()
		}
	}
	t.Fatalf("press pair (gated=%v) did not settle within 400 ticks", gated)
	return 0
}

// TestGateChoreo_OpenValveIsInvisible is the increment-3 gate: with the lane clear,
// a press pair dispatched through the valve never dwells.
//
// The byte-identity assertion the tiered arm used is RETIRED by the uniform shape —
// a gated store's create genuinely differs from a sealed one (it carries a wait
// block instead of a dropoff), by ruling. Tick-identity replaces it: what must not
// regress is the press pair's time to settle, and the mechanism that guarantees it
// is that the tail is appended before the robot reaches the gate, so the wait is
// satisfied on arrival and costs zero ticks of dwell.
//
// Two things are asserted separately, because they can fail independently:
//   - NO DWELL: WaitingOrders() is empty at every tick. This is the real invariant.
//   - TICK PARITY vs the sealed baseline, reported exactly. Any delta here is
//     travel to the wait point, not gate latency.
func TestGateChoreo_OpenValveIsInvisible(t *testing.T) {
	// The gated run costs exactly two structural extras over the sealed shape, and
	// NEITHER is gate latency:
	//
	//   +hopTicks — one coarse cell-step, because the harness models the wait point
	//               as its own plain node. In the plant it sits on the aisle the
	//               robot already drives through to reach the mouth, so this hop is
	//               a scene-modeling artifact, not travel the robot really adds.
	//   +1        — the sim charges one tick to resolve each block, and the gated
	//               order has three blocks where the sealed one has two.
	//
	// So the honest parity statement is delta == hopTicks+1 EXACTLY. Asserting the
	// exact value at two different hop costs is what proves it: a delta that tracks
	// hopTicks is structural, whereas any real dwell would add ticks that do not
	// move with it. A bare "delta <= something" would have hidden that.
	for _, hop := range []int{3, 5} {
		sealed := runPressPair(t, hop, false)
		gated := runPressPair(t, hop, true)
		delta := gated - sealed
		want := hop + 1
		if delta != want {
			t.Errorf("hopTicks=%d: open valve cost %d ticks over the sealed baseline (%d → %d), want exactly %d (one gate-approach hop + one block resolution). A larger delta is real added latency.",
				hop, delta, sealed, gated, want)
		}
		t.Logf("hopTicks=%d: press pair sealed=%d ticks, through an OPEN valve=%d ticks, delta=%d (= hop %d + 1 block) — zero dwell",
			hop, sealed, gated, delta, hop)
	}
}

// TestGateChoreo_ContendedStagesAndDwells pins increment 3's KNOWN-INCOMPLETE
// behavior as expected, rather than working around it.
//
// When the classifier says the lane is contended, the valve creates the unsealed
// waybill and appends nothing — the robot drives to the wait point and holds. The
// release evaluator then appends its tail the moment the lane becomes safe, and
// the store completes wall-free.
//
// Increment 3 asserted only the first half (the dwell), because nothing could
// release it yet. This is the flip: the same dwell now ENDS, and it ends because
// of the blocker's PLACEMENT rather than its completion — the blocker is still in
// the lane driving out when the dwelling robot is let in.
func TestGateChoreo_ContendedStagesThenReleases(t *testing.T) {
	sim := newGatedSim(t, 4, 0) // 0 → the sim's default hop cost
	gc := newGateController(sim, "LANE")

	// Deep blocker and a shallower entrant, different origins. The shallow one is
	// closer, so without the gate it would reach the mouth first and wall the deep
	// one — the owner scenario.
	gc.submit(t, "DEEP", "deep-1", "S1", "", 6)
	gc.submit(t, "SHALLOW", "shallow-1", "S0", "", 1)

	dwelled := false
	ticks, vios, settled := gc.run(t, 400, func() {
		if len(sim.WaitingOrders()) > 0 {
			dwelled = true
		}
	})
	if !settled {
		t.Fatalf("both stores must settle once the gate releases them (ran %d ticks, waiting=%v)", ticks, sim.WaitingOrders())
	}
	if !dwelled {
		t.Fatal("the shallow store should have dwelled at the gate at least one tick — otherwise this scenario is not testing contention")
	}
	for _, v := range vios {
		t.Errorf("contended release fired a checker: %s: %s", v.Checker, v.Detail)
	}
	if n := sim.BusyCount(); n != 0 {
		t.Errorf("walled stores = %d, want 0", n)
	}
	// Deepest-first: the deep bin must be down before the shallow one, or the
	// shallow bin would have walled the deep target.
	if gc.placedAt["deep-1"] >= gc.placedAt["shallow-1"] {
		t.Errorf("deep placed at tick %d, shallow at %d — deepest-first entry was not preserved",
			gc.placedAt["deep-1"], gc.placedAt["shallow-1"])
	}
	// THE claim of this arm: release is driven by PLACEMENT, not COMPLETION. The
	// window is [blocker's bin lands, blocker's robot goes idle] — the blocker is
	// still physically backing out of the lane when the dwelling robot is let in.
	// A completion-coarse gate could only release at or after doneAt.
	rel, placed, done := gc.releasedAt["shallow-1"], gc.placedAt["deep-1"], gc.doneAt["deep-1"]
	if rel < placed {
		t.Errorf("shallow released at tick %d, before the blocker placed at %d — impossible; the model is wrong", rel, placed)
	}
	if done == 0 {
		t.Fatal("blocker completion was never observed — the scenario cannot distinguish placement from completion")
	}
	if rel >= done {
		t.Errorf("shallow released at tick %d but the blocker only finished at %d — that is completion-release, not placement-release (placed at %d)",
			rel, done, placed)
	}
	t.Logf("contended store dwelled, released at tick %d — after the blocker PLACED (%d), before it COMPLETED (%d); settled at %d",
		rel, placed, done, ticks)
}

// TestGateChoreo_ContendedFilterIn: three cross-origin stores converge on one lane
// with scrambled approach distances, so arrival order at the mouth is the REVERSE
// of the safe entry order. The gate has to filter them in deepest-first anyway.
//
// This is the wall scenario generalized past two robots. The reachability checker
// is the real assertion — it fires the moment a bin lands shallower than a target
// something is still bound to — and the placement order pins deepest-first
// explicitly rather than inferring it from the absence of a violation.
func TestGateChoreo_ContendedFilterIn(t *testing.T) {
	sim := newGatedSim(t, 4, 0)
	gc := newGateController(sim, "LANE")

	// Shallowest robot is nearest the lane; deepest is furthest. Ungated, they
	// would enter in exactly the wrong order.
	gc.submit(t, "R-DEEP", "deep", "S2", "", 9)
	gc.submit(t, "R-MID", "mid", "S1", "", 5)
	gc.submit(t, "R-SHALLOW", "shallow", "S0", "", 1)

	ticks, vios, settled := gc.run(t, 600, nil)
	if !settled {
		t.Fatalf("all three stores must settle (ran %d ticks, waiting=%v, busy=%d)", ticks, sim.WaitingOrders(), sim.BusyCount())
	}
	for _, v := range vios {
		t.Errorf("filter-in fired a checker: %s: %s", v.Checker, v.Detail)
	}
	if n := sim.BusyCount(); n != 0 {
		t.Errorf("walled stores = %d, want 0", n)
	}
	// Deepest-first, asserted as a total order over placements.
	got := gc.placementOrder()
	want := []string{"deep", "mid", "shallow"}
	if len(got) != len(want) {
		t.Fatalf("placement order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("placement order = %v, want %v (deepest-first entry is the whole point of the gate)", got, want)
		}
	}
	t.Logf("3-store contended filter-in: placements %v in %d ticks, zero walls, zero checker fires", got, ticks)
}

// TestGateChoreo_Tier1CoRelease: a SAME-ORIGIN pair behind a deeper cross-origin
// blocker must be released TOGETHER — one decision, two appends, same pass.
//
// Tier 1 is the press pair, and splitting it is the one unforgivable regression.
// Releasing only the deeper partner and making the shallower one wait for it to
// place would re-serialize exactly the pair the exemption exists to protect. The
// assertion is therefore on the release TICK being identical, not merely on both
// eventually going.
func TestGateChoreo_Tier1CoRelease(t *testing.T) {
	sim := newGatedSim(t, 4, 0)
	gc := newGateController(sim, "LANE")

	gc.submit(t, "BLOCKER", "blocker", "S2", "", 6) // deeper, cross-origin
	gc.submit(t, "PRESS-L", "press-l", "S1", "press", 2)
	gc.submit(t, "PRESS-R", "press-r", "S0", "press", 2)

	ticks, vios, settled := gc.run(t, 600, nil)
	if !settled {
		t.Fatalf("pair + blocker must settle (ran %d ticks, waiting=%v)", ticks, sim.WaitingOrders())
	}
	for _, v := range vios {
		t.Errorf("Tier-1 co-release fired a checker: %s: %s", v.Checker, v.Detail)
	}
	l, r := gc.releasedAt["press-l"], gc.releasedAt["press-r"]
	if l == 0 || r == 0 {
		t.Fatalf("both partners must have been released by the gate (l=%d r=%d)", l, r)
	}
	if l != r {
		t.Errorf("press partners released at ticks %d and %d — Tier 1 requires ONE decision releasing both, or the pair is re-serialized", l, r)
	}
	// And the pair went on the blocker's PLACEMENT, not its completion — so Tier 1
	// costs the pair nothing beyond the physical wait for the lane to clear.
	placed, done := gc.placedAt["blocker"], gc.doneAt["blocker"]
	if l < placed {
		t.Errorf("pair released at %d, before the blocker placed at %d — impossible", l, placed)
	}
	if done != 0 && l >= done {
		t.Errorf("pair released at %d but the blocker only finished at %d — completion-release, not placement-release", l, done)
	}

	// TICK PARITY vs an OPEN valve. Waiting together must not make the partners
	// wait on EACH OTHER: once released, the pair should interleave exactly as it
	// does with no blocker present. The measure is the gap between their two
	// placements — the open-valve gap is what the press costs inherently, and Tier
	// 1 must reproduce it rather than add a serialization on top.
	openGap, openTicks := pressPairPlacementGap(t)
	gatedGap := gc.placedAt["press-r"] - gc.placedAt["press-l"]
	if gatedGap < 0 {
		gatedGap = -gatedGap
	}
	if gatedGap != openGap {
		t.Errorf("press partners placed %d ticks apart behind a gate vs %d ticks apart through an open valve — Tier 1 added serialization between the pair",
			gatedGap, openGap)
	}
	t.Logf("Tier-1: blocker placed tick %d (done %d), BOTH partners released tick %d, settled %d; pair placement gap %d == open-valve gap %d (open valve settles in %d)",
		placed, done, l, ticks, gatedGap, openGap, openTicks)
}

// pressPairPlacementGap runs the same same-origin pair through an OPEN valve (no
// blocker) and returns how many ticks apart their two bins land, plus the settle
// tick. This is the pair's inherent cost — what Tier 1 must not exceed.
func pressPairPlacementGap(t *testing.T) (gap, ticks int) {
	t.Helper()
	sim := newGatedSim(t, 4, 0)
	gc := newGateController(sim, "LANE")
	gc.submit(t, "PRESS-L", "press-l", "S1", "press", 2)
	gc.submit(t, "PRESS-R", "press-r", "S0", "press", 2)
	ticks, vios, settled := gc.run(t, 400, nil)
	if !settled {
		t.Fatal("open-valve press pair did not settle")
	}
	for _, v := range vios {
		t.Errorf("open-valve press pair fired a checker: %s: %s", v.Checker, v.Detail)
	}
	gap = gc.placedAt["press-r"] - gc.placedAt["press-l"]
	if gap < 0 {
		gap = -gap
	}
	return gap, ticks
}

// TestGateChoreo_DoubleFireIdempotent: evaluating the same lane many times per
// tick must not append a tail twice.
//
// The authoritative guard lives in production (releaseGatedOrder reloads the
// order and re-checks IsGateStaged under the per-lane mutex, asserted in the
// dispatch battery). What this pins is the PHYSICS consequence: a duplicated tail
// would give a robot two dropoff blocks and it would try to place twice, which
// the harness would show as extra work and a bin count that does not match. So
// this runs the evaluator 5× per tick and asserts the outcome is bit-identical to
// running it once.
func TestGateChoreo_DoubleFireIdempotent(t *testing.T) {
	run := func(firesPerTick int) (ticks int, appends map[string]int, bins int) {
		sim := newGatedSim(t, 4, 0)
		gc := newGateController(sim, "LANE")
		gc.firesPerTick = firesPerTick
		gc.submit(t, "DEEP", "deep-1", "S1", "", 6)
		gc.submit(t, "SHALLOW", "shallow-1", "S0", "", 1)
		ticks, vios, settled := gc.run(t, 400, nil)
		if !settled {
			t.Fatalf("firesPerTick=%d: did not settle", firesPerTick)
		}
		for _, v := range vios {
			t.Errorf("firesPerTick=%d fired a checker: %s: %s", firesPerTick, v.Checker, v.Detail)
		}
		n := 0
		for _, s := range []string{"S0", "S1", "S2", "S3"} {
			if sim.HasBin(s) {
				n++
			}
		}
		return ticks, gc.appends, n
	}

	onceTicks, onceAppends, onceBins := run(1)
	manyTicks, manyAppends, manyBins := run(5)

	for id, n := range manyAppends {
		if n != 1 {
			t.Errorf("order %s had its tail appended %d times under repeated evaluation, want exactly 1", id, n)
		}
	}
	if len(manyAppends) != len(onceAppends) {
		t.Errorf("appended orders = %d under 5×/tick vs %d under 1×/tick", len(manyAppends), len(onceAppends))
	}
	if manyTicks != onceTicks || manyBins != onceBins {
		t.Errorf("repeated evaluation changed the outcome: ticks %d→%d, bins %d→%d",
			onceTicks, manyTicks, onceBins, manyBins)
	}
	t.Logf("double-fire: 5 evaluations/tick produced an identical run (%d ticks, %d bins, one append per order)", manyTicks, manyBins)
}

// TestGateChoreo_SoakZeroWalls is the 200-seed soak with the gate armed: random
// pre-fill, random approach distances, cross-origin stores filling whatever the
// lane has left. Every seed must settle wall-free with every checker green.
//
// It is the direct counterpart of TestTieredEntry_SoakZeroWalls, which soaks the
// fallback arm over the same configurations — so the two arms are held to the
// same bar on the same shapes.
func TestGateChoreo_SoakZeroWalls(t *testing.T) {
	const seeds = 200
	const slots = 4
	var walled, violated, dwelt int

	for seed := range seeds {
		rng := rand.New(rand.NewSource(int64(seed)))
		f := rng.Intn(slots - 1)
		var preFill []string
		for i := slots - f; i < slots; i++ {
			preFill = append(preFill, fmt.Sprintf("S%d", i))
		}
		nEmpty := slots - f

		sim := newGatedSim(t, slots, 0)
		for _, s := range preFill {
			sim.PlaceBin(s)
		}
		gc := newGateController(sim, "LANE")
		for k := range nEmpty {
			depthIdx := (nEmpty - 1) - k
			gc.submit(t, fmt.Sprintf("R%d", depthIdx), fmt.Sprintf("o%d", depthIdx),
				fmt.Sprintf("S%d", depthIdx), "", 1+rng.Intn(8))
		}

		_, vios, settled := gc.run(t, 800, nil)
		if !settled || sim.BusyCount() > 0 {
			walled++
		}
		if len(vios) > 0 {
			violated++
		}
		if len(gc.releasedAt) > 0 {
			dwelt++
		}
	}
	if walled > 0 || violated > 0 {
		t.Errorf("gated-lane soak must be clean: %d/%d seeds walled, %d/%d fired a checker", walled, seeds, violated, seeds)
	}
	t.Logf("GATED-LANE soak: %d/%d walled, %d/%d fired a checker, %d/%d seeds exercised a real gate release — clean",
		walled, seeds, violated, seeds, dwelt, seeds)
}
