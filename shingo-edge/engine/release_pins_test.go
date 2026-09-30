package engine

// release_pins_test.go — the named pins: the live bugs S1 fixes, the behaviour
// S1 must keep, and the multi-step choreographies a single door click cannot
// show. Same runner and outcome as the matrix (release_matrix_test.go); these
// cells differ only in driving more than one event.

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/uop"
)

// restart replaces the engine with a fresh one over the same database — what an
// Edge restart does to everything held in memory (the pair-deferral map, the
// survivor bound). The fake Core and the curtain script carry over.
func (h *relHarness) restart() {
	h.t.Helper()
	eng := testEngine(h.t, h.db)
	eng.orderMgr = orders.NewManager(h.db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()
	mut := uop.New(h.db, "test.station", h.db, h.db)
	eng.SetInventoryDeltaSink(mut)
	eng.coreClient = h.eng.coreClient
	eng.plcMgr = h.eng.plcMgr
	h.eng, h.mutator = eng, mut
	h.handler = newEdgeHandlerFor(eng)
}

// pickedUp delivers Core's BinPickedUp for a leg lifting the front node's bin.
func (h *relHarness) pickedUp(leg string) {
	h.t.Helper()
	h.eng.HandleBinPickedUp(h.order(leg).UUID, fxBin, fxPress)
}

// seq runs acts in order and returns the last error (earlier errors are fatal:
// a pin's setup acts are not what it is about).
func seq(acts ...func(h *relHarness) error) func(h *relHarness) error {
	return func(h *relHarness) error {
		var err error
		for i, a := range acts {
			err = a(h)
			if err != nil && i < len(acts)-1 {
				h.t.Fatalf("setup act %d: %v", i, err)
			}
		}
		return err
	}
}

func refuse(leg, code string) func(h *relHarness) error {
	return func(h *relHarness) error { h.coreRefuses(leg, code); return nil }
}

func stages(leg string) func(h *relHarness) error {
	return func(h *relHarness) error { h.coreStages(leg); return nil }
}

func confirms(leg string) func(h *relHarness) error {
	return func(h *relHarness) error { h.coreConfirms(leg); return nil }
}

func picks(leg string) func(h *relHarness) error {
	return func(h *relHarness) error { h.pickedUp(leg); return nil }
}

func restarts() func(h *relHarness) error {
	return func(h *relHarness) error { h.restart(); return nil }
}

func pPile(h *relHarness) []string { return []string{fmt.Sprintf("pile=%d", h.pile(fxPart))} }
func pPull(h *relHarness) []string { return []string{h.activePull()} }

// pBin is the front node's bound bin.
func pBin(h *relHarness) []string {
	rt, err := h.db.GetProcessNodeRuntime(h.nodeID)
	testutil.MustNoErr(h.t, err, "runtime")
	if rt.ActiveBinID == nil {
		return []string{"bin=nil"}
	}
	return []string{fmt.Sprintf("bin=%d", *rt.ActiveBinID)}
}

// pMoves counts the live move orders at the front node — what the Material
// page's RELEASE creates.
func pMoves(h *relHarness) []string {
	os, err := h.db.ListActiveOrdersByProcessNodeAndType(h.nodeID, orders.TypeMove)
	testutil.MustNoErr(h.t, err, "list moves")
	return []string{fmt.Sprintf("moves=%d", len(os))}
}

// materialTap is the Material page's RELEASE with the count the operator typed.
func materialTap(remaining int) func(h *relHarness) error {
	return func(h *relHarness) error {
		_, err := h.eng.ReleaseNodeWithRemainingUOP(h.nodeID, 1, remaining)
		return err
	}
}

// positionEvac is door 4's building block: releaseNodeWithClaim with a
// fallback claim, which only EvacuateNode's no-OutboundStaging arm reaches (a
// fanned-out press position that has no claim row under its own name).
func positionEvac(h *relHarness) error {
	node, err := h.db.GetProcessNode(h.nodeID)
	testutil.MustNoErr(h.t, err, "node")
	_, err = h.eng.releaseNodeWithClaim(h.nodeID, 1, nil, requestedClaimAtNode(h.db, node))
	return err
}

// deliverBack puts the back position's changeover order in delivered — the
// state flipTargetReady reads as "this side can feed the line".
func deliverBack(h *relHarness) {
	h.t.Helper()
	tasks, err := h.db.ListChangeoverNodeTasks(h.coID)
	testutil.MustNoErr(h.t, err, "tasks")
	for _, tk := range tasks {
		if tk.ProcessNodeID == h.partnerID && tk.NextMaterialOrderID != nil {
			testutil.MustNoErr(h.t, h.db.UpdateOrderStatus(*tk.NextMaterialOrderID, string(protocol.StatusDelivered)), "deliver back")
			return
		}
	}
	h.t.Fatal("fixture: no back-position changeover order")
}

func TestReleasePins(t *testing.T) {
	t.Parallel()
	runRelCells(t, releasePinCells())
}

func releasePinCells() []relCell {
	twoRobot := pairSpec{mode: protocol.SwapModeTwoRobot}
	twoRobotConsume := pairSpec{mode: protocol.SwapModeTwoRobot, role: protocol.ClaimRoleConsume}
	coPI := coSpec{mode: protocol.SwapModeTwoRobotPressIndex}
	coPITooling := coSpec{mode: protocol.SwapModeTwoRobotPressIndex, tooling: true}
	coSRTooling := coSpec{mode: protocol.SwapModeSingleRobot, tooling: true}
	coTwoRobot := coSpec{mode: protocol.SwapModeTwoRobot}
	// A 3-position press marked for tooling at the FRONT position only: the
	// pair survives the decorator, the evac (R1) carries "ready" and "tooling
	// done", and the index leg (R2) carries "ready" and a staging hold before it
	// sets its bin on the press (holdComplexInbound).
	coPI3MarkedFront := coSpec{mode: protocol.SwapModeTwoRobotPressIndex, threePos: true, markedOnly: []string{fxPress}}
	S, D, T := protocol.StatusStaged, protocol.StatusDispatched, protocol.StatusInTransit
	withBinAt := func(b func(h *relHarness)) func(h *relHarness) {
		return func(h *relHarness) {
			b(h)
			h.core.binAt = &NodeBinInfo{NodeName: fxPress, BinID: fxBin, PayloadCode: fxPart, UOPRemaining: fxCount, Occupied: true}
		}
	}

	return []relCell{
		// ── N-a: a second release on an in_transit multi-wait leg ─────────
		// The Edge sends it; Core appends the NEXT wait's segment (the Core
		// half is pinned in shingo-core). The Edge half is characterised here:
		// the envelope goes, through the sweep the page's own toast invites.
		{name: "N-a/sweep twice on a single_robot tooling evac",
			want:  "ok | evac=in_transit supply=staged | rel=evac,evac | ingest=0 capred=0 | sweep released=1 pending=1 flip=[] | sweep released=1 pending=1 flip=[] | uop=42",
			build: coAt(coSRTooling, "evac", S, "supply", S),
			act:   seq(sweepClick(dispNone), sweepClick(dispNone)), probe: probes(pUOP)},
		{name: "N-a/station button twice on a press-index tooling pair",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac,supply | ingest=0 capred=0 | uop=42",
			build: coAt(coPITooling, "evac", S, "supply", S),
			act:   seq(pairClick(dispNone), pairClick(dispNone)), probe: probes(pUOP)},
		{name: "N-a/sweep twice on a press-index tooling R1",
			want:  "ok | evac=in_transit supply=staged | rel=evac,evac | ingest=0 capred=0 | sweep released=1 pending=1 flip=[] | sweep released=1 pending=1 flip=[] | uop=42",
			build: coAt(coPITooling, "evac", S, "supply", S),
			act:   seq(sweepClick(dispNone), sweepClick(dispNone)), probe: probes(pUOP)},

		// ── N-a′: the pickup chain releases a supply the pair click released ─
		{name: "N-a'/pickup chain, R2 still driving to its hold",
			bug:   "N-a'",
			today: "ok | evac=in_transit supply=in_transit | rel=evac,supply,supply | ingest=0 capred=0",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=0 capred=0",
			build: coAt(coPI3MarkedFront, "evac", S, "supply", S),
			act:   seq(pairClick(dispNone), picks("evac"))},
		{name: "N-a'/pickup chain, R2 already parked at its hold",
			bug:   "N-a'",
			today: "ok | evac=in_transit supply=in_transit | rel=evac,supply,supply | ingest=0 capred=0",
			want:  "ok | evac=in_transit supply=staged | rel=evac,supply | ingest=0 capred=0",
			build: coAt(coPI3MarkedFront, "evac", S, "supply", S),
			act:   seq(pairClick(dispNone), stages("supply"), picks("evac"))},
		// KEEP: the sweep defers the holdInbound supply to the evac's lift.
		{name: "keep/sweep defers the held supply to the evac's pickup",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=0 capred=0 | sweep released=1 pending=1 flip=[]",
			build: coAt(coPI3MarkedFront, "evac", S, "supply", S),
			act:   seq(sweepClick(dispNone), picks("evac"))},
		{name: "keep/two_robot changeover supply released at the evac's pickup",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=0 capred=0 | sweep released=1 pending=1 flip=[]",
			build: coAt(coTwoRobot, "evac", S, "supply", S),
			act:   seq(sweepClick(dispNone), picks("evac"))},

		// ── L1: the survivor arm releases waits nobody clicked ─────────────
		{name: "L1/single_robot relay: stage leg confirms, swap leg at ready",
			bug:   "L1",
			today: "ok | evac=in_transit supply=confirmed | rel=evac | ingest=0 capred=0",
			want:  "ok | evac=staged supply=confirmed | rel=- | ingest=0 capred=0",
			build: coAt(coSRTooling, "evac", S, "supply", T),
			act:   confirms("supply")},
		{name: "L1/press-index tooling: R2 confirms, R1 at tooling done",
			bug:   "L1",
			today: "ok | evac=in_transit supply=confirmed | rel=evac | ingest=0 capred=0",
			want:  "ok | evac=staged supply=confirmed | rel=- | ingest=0 capred=0",
			build: coAt(coPITooling, "evac", S, "supply", T),
			act:   confirms("supply")},
		// KEEP: the steady-state survivor (run 12d order 84) still goes.
		{name: "keep/two_robot survivor: evac confirms, supply staged",
			want:  "ok | evac=confirmed supply=in_transit | rel=supply | ingest=0 capred=0",
			build: pairAt(twoRobot, "evac", T, "supply", S),
			act:   confirms("evac")},
		// L1's companion: once the survivor stops releasing it, the held supply
		// needs the tooling-done click to cover it.
		{name: "L1c/tooling done sweep, R2 parked at its hold",
			bug:   "L1c",
			today: "ok | evac=in_transit supply=staged | rel=evac,supply,evac | ingest=0 capred=0 | sweep released=1 pending=1 flip=[]",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac,supply | ingest=0 capred=0 | sweep released=2 pending=0 flip=[]",
			build: coAt(coPI3MarkedFront, "evac", S, "supply", S),
			act:   seq(pairClick(dispNone), stages("evac"), stages("supply"), sweepClick(dispNone))},

		// ── The press-index pair (keep) ──────────────────────────────────────
		{name: "keep/pi changeover pair, both staged",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=0 capred=0",
			build: coAt(coPI, "evac", S, "supply", S),
			act:   pairClick(dispNone)},

		// ── Door 8: the deferred re-fire ─────────────────────────────────────
		{name: "d8/two_robot supply stages after the click",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0",
			build: pairAt(twoRobot, "evac", S, "supply", D),
			act:   seq(pairClick(dispEmpty), stages("supply")), probe: probes(pDeferred)},
		{name: "d8/two_robot re-fire into a live curtain",
			bug:   "A6",
			today: "ok | evac=in_transit supply=staged | rel=evac | ingest=1 capred=0 | chip:supply=-",
			want:  "ok | evac=in_transit supply=staged | rel=evac | ingest=1 capred=0 | chip:supply=Release the light curtain at SYN-PRESS, then press RELEASE again.",
			build: withCurtain(curtainSafe, pairAt(twoRobot, "evac", S, "supply", D)),
			act: seq(pairClick(dispEmpty), func(h *relHarness) error { h.wl.set(curtainLive); return nil },
				stages("supply")),
			probe: probes(pDeferred, pChip("supply"))},
		// ── Door 9: the survivor into a live curtain ─────────────────────────
		{name: "d9/two_robot survivor into a live curtain",
			bug:   "A6",
			today: "ok | evac=confirmed supply=staged | rel=- | ingest=0 capred=0 | chip:supply=-",
			want:  "ok | evac=confirmed supply=staged | rel=- | ingest=0 capred=0 | chip:supply=Release the light curtain at SYN-PRESS, then press RELEASE again.",
			build: withCurtain(curtainLive, pairAt(twoRobot, "evac", T, "supply", S)),
			act:   confirms("evac"), probe: probes(pChip("supply"))},
		// ── Door 10: the pickup chain into a live curtain ────────────────────
		{name: "d10/changeover supply at the evac's pickup, curtain live",
			bug:   "curtain-exempt",
			today: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=0 capred=0 | sweep released=1 pending=1 flip=[] | chip:supply=-",
			want:  "ok | evac=in_transit supply=staged | rel=evac | ingest=0 capred=0 | sweep released=1 pending=1 flip=[] | chip:supply=Release the light curtain at SYN-PRESS, then press RELEASE again.",
			build: withCurtain(curtainSafe, coAt(coTwoRobot, "evac", S, "supply", S)),
			act: seq(sweepClick(dispNone), func(h *relHarness) error { h.wl.set(curtainLive); return nil },
				picks("evac")),
			probe: probes(pChip("supply"))},

		// ── L2: capture once (P-E1..P-E6) ────────────────────────────────────
		{name: "L2/P-E1 refused, clicked again with the same qty",
			bug:   "L2",
			today: "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac,supply | ingest=0 capred=-10 | pile=10 | chip:evac=-",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac,supply | ingest=0 capred=-5 | pile=5 | chip:evac=-",
			build: withBinAt(pairAt(twoRobotConsume, "evac", S, "supply", S)),
			act:   seq(pairClick(dispPull(5)), refuse("evac", "invalid_state"), pairClick(dispPull(5))),
			probe: probes(pPile, pChip("evac"))},
		{name: "L2/P-E2 refused, clicked again with a larger qty",
			bug:   "L2",
			today: "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac,supply | ingest=0 capred=-13 | pile=13",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac,supply | ingest=0 capred=-8 | pile=8",
			build: withBinAt(pairAt(twoRobotConsume, "evac", S, "supply", S)),
			act:   seq(pairClick(dispPull(5)), refuse("evac", "invalid_state"), pairClick(dispPull(8))),
			probe: probes(pPile)},
		{name: "L2/P-E3 refused, Edge restarts, clicked again",
			bug:   "L2",
			today: "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac,supply | ingest=0 capred=-10 | pile=10",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac,supply | ingest=0 capred=-5 | pile=5",
			build: withBinAt(pairAt(twoRobotConsume, "evac", S, "supply", S)),
			act:   seq(pairClick(dispPull(5)), refuse("evac", "invalid_state"), restarts(), pairClick(dispPull(5))),
			probe: probes(pPile)},
		{name: "L2/P-E4 refused on a gate wait, clicked again",
			bug:   "L2",
			today: "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac,supply | ingest=0 capred=-10 | pile=10",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac,supply | ingest=0 capred=-5 | pile=5",
			build: withBinAt(pairAt(twoRobotConsume, "evac", S, "supply", S)),
			act:   seq(pairClick(dispPull(5)), refuse("evac", "invalid_state"), pairClick(dispPull(5))),
			probe: probes(pPile)},
		{name: "L2/repeat with no refusal: per-order click on an in_transit evac",
			bug:   "L2",
			today: "ok | evac=in_transit supply=dispatched | rel=evac,evac | ingest=0 capred=-10 | pile=10",
			want:  "ok | evac=in_transit supply=dispatched | rel=evac,evac | ingest=0 capred=-5 | pile=5",
			build: withBinAt(pairAt(twoRobotConsume, "evac", S, "supply", D)),
			act:   seq(orderClick("evac", dispPull(5)), orderClick("evac", dispPull(5))),
			probe: probes(pPile)},
		{name: "L2/P-E5 refused Z with a deferred Y: one extra Y round trip, then nothing",
			want:  "ok | evac=staged supply=staged | rel=evac,supply | ingest=0 capred=-5",
			build: withBinAt(pairAt(twoRobotConsume, "evac", S, "supply", D)),
			act: seq(pairClick(dispPull(5)), refuse("evac", "invalid_state"), stages("supply"),
				refuse("supply", "invalid_state"))},
		{name: "L2/P-E6 accepted capture",
			want: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=0 capred=-5 | pile=5 | env:evac uop=nil kind=pull_parts | env:supply uop=nil kind=-", build: withBinAt(pairAt(twoRobotConsume, "evac", S, "supply", S)),
			act: pairClick(dispPull(5)), probe: probes(pPile, pEnv)},
		{name: "L2/P-E6 accepted release empty",
			want: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=0 capred=0 | uop=0 | env:evac uop=0 kind=release_empty | env:supply uop=nil kind=-", build: pairAt(twoRobotConsume, "evac", S, "supply", S),
			act: pairClick(dispEmpty), probe: probes(pUOP, pEnv)},
		{name: "L2/P-E6 accepted send partial",
			want: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=0 capred=0 | uop=5 | env:evac uop=5 kind=release_partial/5 | env:supply uop=nil kind=-", build: pairAt(twoRobotConsume, "evac", S, "supply", S),
			act: pairClick(dispPartial(5)), probe: probes(pUOP, pEnv)},

		// ── N3 (Edge half): what the Edge does with each release-path code ──
		{name: "N3/invalid_state",
			want: "ok | evac=staged supply=dispatched | rel=evac | ingest=0 capred=0 | chip:evac=Core rejected the release", build: pairAt(twoRobot, "evac", S, "supply", D),
			act: seq(orderClick("evac", dispEmpty), refuse("evac", "invalid_state")), probe: probes(pChip("evac"))},
		{name: "N3/manifest_sync_failed",
			want: "ok | evac=staged supply=dispatched | rel=evac | ingest=0 capred=0 | chip:evac=Manifest sync failed at Core", build: pairAt(twoRobot, "evac", S, "supply", D),
			act: seq(orderClick("evac", dispEmpty), refuse("evac", "manifest_sync_failed")), probe: probes(pChip("evac"))},
		{name: "N3/fleet_failed",
			want: "ok | evac=failed supply=dispatched | rel=evac | ingest=0 capred=0 | chip:evac=-", build: pairAt(twoRobot, "evac", S, "supply", D),
			act: seq(orderClick("evac", dispEmpty), refuse("evac", "fleet_failed")), probe: probes(pChip("evac"))},
		{name: "N3/internal_error",
			want: "ok | evac=failed supply=dispatched | rel=evac | ingest=0 capred=0 | chip:evac=-", build: pairAt(twoRobot, "evac", S, "supply", D),
			act: seq(orderClick("evac", dispEmpty), refuse("evac", "internal_error")), probe: probes(pChip("evac"))},

		// ── N4: a refused produce evac ───────────────────────────────────────
		// A ruling, not a bug (owner, 2026-09-30): the produce count splits at
		// the operator's RELEASE. The departing bin keeps the 42 it shipped
		// with, and ticks after the click belong to the next bin, so a Core
		// refusal of the evac changes neither.
		{name: "N4/produce pair click refused by Core",
			want:  "ok | evac=staged supply=staged | rel=evac,supply | ingest=1 capred=0 | uop=0 | bin=nil",
			build: pairAt(twoRobot, "evac", S, "supply", S),
			act:   seq(pairClick(dispEmpty), refuse("evac", "invalid_state"), refuse("supply", "invalid_state")),
			probe: probes(pUOP, pBin)},

		// ── L5 / L4: the produce paperwork ───────────────────────────────────
		{name: "L5/unchanged neighbour's pair during another node's changeover",
			bug:   "L5",
			today: "ok | nevac=in_transit nsupply=in_transit evac=submitted supply=submitted | rel=nevac,nsupply | ingest=0 capred=0",
			want:  "ok | nevac=in_transit nsupply=in_transit evac=submitted supply=submitted | rel=nevac,nsupply | ingest=1 capred=0",
			build: func(h *relHarness) {
				h.changeover(coSpec{mode: protocol.SwapModeTwoRobot, neighbour: true})
				statuses(h, "nevac", S, "nsupply", S)
			},
			act: func(h *relHarness) error { return h.eng.ReleaseStagedOrders(h.partnerID, dispEmpty) }},
		{name: "L4/per-order release of a departing produce leg",
			bug:   "L4",
			today: "ok | evac=in_transit supply=dispatched | rel=evac | ingest=0 capred=0 | uop=42 | bin=9001",
			want:  "ok | evac=in_transit supply=dispatched | rel=evac | ingest=1 capred=0 | uop=0 | bin=nil",
			build: pairAt(twoRobot, "evac", S, "supply", D),
			act:   orderClick("evac", dispEmpty), probe: probes(pUOP, pBin)},
		{name: "L4/per-order release of the placing produce leg ships nothing",
			want:  "ok | evac=in_transit supply=in_transit | rel=supply | ingest=0 capred=0 | uop=42 | bin=9001",
			build: pairAt(twoRobot, "evac", T, "supply", S),
			act:   orderClick("supply", dispNone), probe: probes(pUOP, pBin)},

		// ── Door 3: the Material page ────────────────────────────────────────
		{name: "d3/material release",
			want:  "ok |  | rel=- | ingest=0 capred=0 | moves=1 | uop=42",
			build: func(h *relHarness) { h.seedNode(twoRobot) },
			act:   materialTap(0), probe: probes(pMoves, pUOP)},
		{name: "d3/material release tapped twice",
			bug:   "L8",
			today: "ok |  | rel=- | ingest=0 capred=0 | moves=2",
			want:  "refuse:in-flight |  | rel=- | ingest=0 capred=0 | moves=1",
			build: func(h *relHarness) { h.seedNode(twoRobot) },
			act:   seq(materialTap(0), materialTap(0)), probe: probes(pMoves)},
		{name: "d3/material release, curtain live",
			want:  "refuse:curtain |  | rel=- | ingest=0 capred=0 | moves=0",
			build: func(h *relHarness) { h.seedNode(twoRobot); h.armCurtain(curtainLive) },
			act:   materialTap(0), probe: probes(pMoves)},
		// ── Door 4: the changeover position evac (fallback claim) ────────────
		{name: "d4/position evac, curtain live",
			bug:   "curtain-exempt",
			today: "ok |  | rel=- | ingest=0 capred=0 | moves=1",
			want:  "refuse:curtain |  | rel=- | ingest=0 capred=0 | moves=0",
			build: func(h *relHarness) { h.seedNode(twoRobot); h.armCurtain(curtainLive) },
			act:   positionEvac, probe: probes(pMoves)},

		// ── L3: one press on a sequential A/B press ──────────────────────────
		// fact-owners Lane G (under this branch) already flips onto a ready
		// partner. The line always goes to the partner (owner, 2026-09-30), so a
		// partner that is not ready is the bug, and the sweep's decline is the
		// 2026-08-28 rule Lane G dropped.
		{name: "L3/per-order release at the pulled side, partner ready",
			want:  "ok | removal=in_transit | rel=removal | ingest=0 capred=0 | pull=SYN-PRESS-B",
			build: func(h *relHarness) { h.sequentialAB(protocol.ClaimRoleConsume, true); statuses(h, "removal", S) },
			act:   orderClick("removal", dispEmpty), probe: probes(pPull)},
		{name: "L3/per-order release at the pulled side, partner not ready",
			bug:   "L3",
			today: "refuse:pull | removal=staged | rel=- | ingest=0 capred=0 | pull=SYN-PRESS",
			want:  "ok | removal=in_transit | rel=removal | ingest=0 capred=0 | pull=SYN-PRESS-B",
			build: func(h *relHarness) { h.sequentialAB(protocol.ClaimRoleConsume, false); statuses(h, "removal", S) },
			act:   orderClick("removal", dispEmpty), probe: probes(pPull)},
		{name: "L3/changeover node click at the pulled side, partner ready",
			want: "ok | supply=in_transit | rel=supply | ingest=0 capred=0 | node released=1 pending=0 flip=[] | pull=SYN-PRESS-B",
			build: func(h *relHarness) {
				h.coID = h.changeover(coSpec{mode: protocol.SwapModeSequential})
				statuses(h, "supply", S)
				deliverBack(h)
			},
			act: nodeCOClick(dispNone), probe: probes(pPull)},
		{name: "L3/changeover node click at the pulled side, partner not ready",
			bug:   "L3-co",
			today: "refuse:pull | supply=staged | rel=- | ingest=0 capred=0 | node released=0 pending=0 flip=[] | pull=SYN-PRESS",
			want:  "ok | supply=in_transit | rel=supply | ingest=0 capred=0 | node released=1 pending=0 flip=[] | pull=SYN-PRESS-B",
			build: func(h *relHarness) {
				h.coID = h.changeover(coSpec{mode: protocol.SwapModeSequential})
				statuses(h, "supply", S)
			},
			act: nodeCOClick(dispNone), probe: probes(pPull)},
		{name: "L3/sweep declines the pulled side",
			bug:   "L3-sweep",
			today: "ok | supply=in_transit | rel=supply | ingest=0 capred=0 | sweep released=1 pending=1 flip=[SYN-PRESS-B] | pull=SYN-PRESS-B",
			want:  "ok | supply=staged | rel=- | ingest=0 capred=0 | sweep released=0 pending=2 flip=[SYN-PRESS] | pull=SYN-PRESS",
			build: func(h *relHarness) {
				h.coID = h.changeover(coSpec{mode: protocol.SwapModeSequential})
				statuses(h, "supply", S)
				deliverBack(h)
			},
			act: sweepClick(dispNone), probe: probes(pPull)},
	}
}
