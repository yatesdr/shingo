package scenesim

import (
	"testing"

	"shingocore/fleet"
)

// S1 — the two wound reproductions. These are the measuring stick for the lane
// seam fixes (stages 5–6): today, with NO gate and NO deepest-first discipline,
// each wound reproduces and its checker fires. The assertions are INVERTED (◆):
// they assert the wound is PRESENT under today's behavior, so CI stays green
// while the red evidence is actively proven. When the fix lands, flip each to
// assert the checker stays CLEAN — that is the green measuring stick.

func hasChecker(vs []Violation, name string) bool {
	for _, v := range vs {
		if v.Checker == name {
			return true
		}
	}
	return false
}

func ticksCollect(sim *Sim, n int) []Violation {
	var out []Violation
	for range n {
		out = append(out, sim.Tick()...)
	}
	return out
}

// carryStoreWait: pick at the source, drop at a lane slot, then hold there (a
// swap that dropped mid-lane and is about to come back out).
func carryStoreWait(id, source, slot string) fleet.CreateOrderRequest {
	return fleet.CreateOrderRequest{
		OrderID: id,
		Blocks: []fleet.OrderBlock{
			{BlockID: id + "-b1", Location: source, BinTask: "JackLoad"},
			{BlockID: id + "-b2", Location: slot, BinTask: "JackUnload"},
			{BlockID: id + "-b3", Location: slot, BinTask: "Wait"},
		},
	}
}

// TestS1_HeadOnDeadlock reproduces the Hopkinsville incident: a swap in the lane
// (dropped mid-depth, about to come out) and a store driving in for a deeper slot
// meet head-on in the single-file aisle and neither can move. Both are INBOUND,
// so this is a PHYSICAL deadlock, not a mode collision — mode purity must stay
// clean while no-deadlock fires.
func TestS1_HeadOnDeadlock(t *testing.T) {
	sc := plantScene(t) // depth-3 lane A1: S0(mouth) S1 S2
	sim := New(sc, Options{Watchdog: 40})
	_ = sim.AddRobot("SWAP", "AISLE")
	_ = sim.AddRobot("STORE", "AISLE")

	// The swap drops at S1 (mid-lane) and holds — it is "in there, coming out".
	_ = sim.Submit("SWAP", carryStoreWait("swap-1", "LINE-IN", "A1-S1"), false)
	vio := ticksCollect(sim, 80)

	// The store drives in for the deep slot S2, behind the swap.
	_ = sim.Submit("STORE", storeReq("store-1", "LINE-IN", "A1-S2"), false)
	vio = append(vio, ticksCollect(sim, 80)...)

	// The swap comes out — straight into the store. Head-on.
	sim.ReleaseWait("swap-1")
	vio = append(vio, ticksCollect(sim, 80)...)

	// ◆ INVERTED: today (no gate) this deadlocks. Flip to assert-clean when the
	// mouth gate lands (stages 5–6 / S2-green).
	if !hasChecker(vio, "no-deadlock") {
		t.Fatal("expected a head-on deadlock under today's behavior (no gate); none detected")
	}
	if hasChecker(vio, "mode-purity") {
		t.Error("head-on is same-mode (both inbound) — mode purity must not fire")
	}
}

// TestS1_EntryOrderAirBubble reproduces the §13.4 wound: two inbound stores bind
// slots at sourcing, but the SHALLOW one enters first and drops its bin, walling
// off the DEEP one's bound slot. Nobody steals it; the deep store can never reach
// its slot. The reachability checker fires.
func TestS1_EntryOrderAirBubble(t *testing.T) {
	sc := plantScene(t)
	sim := New(sc, Options{Watchdog: 400})
	_ = sim.AddRobot("SHALLOW", "AISLE")
	_ = sim.AddRobot("DEEP", "AISLE")

	// Entry order inverts depth order: the shallow store (S1) goes in first and
	// drops, walling the lane behind it.
	_ = sim.Submit("SHALLOW", storeReq("shallow-1", "LINE-IN", "A1-S1"), false)
	vio := ticksCollect(sim, 100)

	// The deep store, bound to S2 at sourcing, now cannot reach its slot.
	_ = sim.Submit("DEEP", storeReq("deep-1", "LINE-IN", "A1-S2"), false)
	vio = append(vio, ticksCollect(sim, 100)...)

	// ◆ INVERTED: today (no deepest-first discipline) the deep bind is walled.
	// Flip to assert-clean when deepest-first parking lands (stage 5 / S2-green).
	if !hasChecker(vio, "reachability") {
		t.Fatal("expected the deep bind to be walled off (air bubble) under today's behavior; none detected")
	}
}
