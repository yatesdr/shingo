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
	"shingoedge/release"
	"shingoedge/uop"
)

// restart replaces the engine with a fresh one over the same database — what an
// Edge restart does to everything held in memory. Intents are on the order
// row and survive it. The fake Core and the curtain script carry over.
func (h *relHarness) restart() {
	h.t.Helper()
	eng := testEngine(h.t, h.db)
	eng.orderMgr = orders.NewManager(h.db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()
	mut := uop.New(h.db, "test.station", h.db, h.db)
	eng.SetInventoryDeltaSink(mut)
	eng.coreClient = h.eng.coreClient
	eng.points = nil
	eng.plcMgr = h.eng.plcMgr
	h.eng, h.mutator = eng, mut
	h.handler = newEdgeHandlerFor(eng)
}

// pickedUp delivers Core's BinPickedUp for a leg lifting the front node's bin.
func (h *relHarness) pickedUp(leg string) {
	h.t.Helper()
	h.lifted[fxPress] = true
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

// stagesAt delivers Core's OrderStaged for a leg parked at its station wait
// with this ordinal (the number Core reports, S3).
func stagesAt(leg string, ordinal int) func(h *relHarness) error {
	return func(h *relHarness) error { h.coreStagesAt(leg, ordinal); return nil }
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

// boundarySpy passes every call through to the real sink and counts the
// attribution boundaries marked, per node.
type boundarySpy struct {
	InventoryDeltaSink
	marked map[int64]int
}

func (s *boundarySpy) MarkAttributionBoundary(nodeID int64) error {
	s.marked[nodeID]++
	return s.InventoryDeltaSink.MarkAttributionBoundary(nodeID)
}

// pBoundary reports how many attribution boundaries the act marked on the
// partner position.
func pBoundary(h *relHarness) []string {
	spy := h.eng.inventoryDelta.(*boundarySpy)
	return []string{fmt.Sprintf("boundary=%d", spy.marked[h.partnerID])}
}

// seqWithCurtain builds a sequential produce pair with a staged removal, the
// front position curtained at the given reading, and the attribution
// boundaries counted.
func seqWithCurtain(reading bool) func(h *relHarness) {
	return func(h *relHarness) {
		h.sequentialAB(protocol.ClaimRoleProduce, true)
		statuses(h, "removal", protocol.StatusStaged)
		h.armCurtain(reading)
		h.eng.SetInventoryDeltaSink(&boundarySpy{InventoryDeltaSink: h.mutator, marked: map[int64]int{}})
	}
}

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
		// the envelope goes, through a second sweep (which the page's toast
		// invited until it reported deferred supply apart from pending).
		{name: "N-a/sweep twice on a single_robot tooling evac",
			want:  "ok | evac=in_transit supply=staged | rel=evac | ingest=1 capred=0 | sweep released=1 pending=0 deferred=0 flip=[] | sweep released=0 pending=0 deferred=0 flip=[] | uop=0",
			build: coAt(coSRTooling, "evac", S, "supply", S),
			act:   seq(sweepClick(dispNone), sweepClick(dispNone)), probe: probes(pUOP)},
		{name: "N-a/station button twice on a press-index tooling pair",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | uop=0",
			build: coAt(coPITooling, "evac", S, "supply", S),
			act:   seq(pairClick(dispNone), pairClick(dispNone)), probe: probes(pUOP)},
		{name: "N-a/sweep twice on a press-index tooling R1",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | sweep released=2 pending=0 deferred=0 flip=[] | sweep released=0 pending=0 deferred=0 flip=[] | uop=0",
			build: coAt(coPITooling, "evac", S, "supply", S),
			act:   seq(sweepClick(dispNone), sweepClick(dispNone)), probe: probes(pUOP)},

		// ── N-a(ii): a second click on an evac already staged at "tooling done" ─
		// N-a covers a leg still driving between its waits. Once the evac has
		// staged at its later wait, a second click meant for "ready" releases
		// it past the tooling hold: the Edge cannot tell the two clicks apart,
		// because the act does not say which wait it is for. S5's
		// purpose-scoped act is the fix; until then the plant instruction
		// stands (no second RELEASE on a tooling node before tooling is done).
		{name: "N-a(ii)/ready clicked again after the evac staged at tooling done",
			want:  "ok | evac=staged supply=staged | rel=evac | ingest=1 capred=0 | sweep released=1 pending=0 deferred=0 flip=[] | sweep released=0 pending=0 deferred=0 flip=[]",
			build: coAt(coSRTooling, "evac", S, "supply", S),
			act: seq(sweepClickAs(release.PurposeReady, dispNone), stagesAt("evac", 1),
				sweepClickAs(release.PurposeReady, dispNone))},

		// ── §6.4: the station button on a single_robot changeover node ────────
		// Its relay task carries both legs, and the pair path refuses any mode
		// but a two-robot swap. The adapters route a changeover node's click
		// to the changeover act unless its work is a two-robot swap (S4), which
		// releases the evac and defers the supply, as the sweep and the
		// per-node click do. The ingest is S1b's rule: a changeover produce
		// evac ships its bin's count at RELEASE.
		{name: "6.4/station RELEASE on a single_robot changeover node",
			want:  "ok | evac=in_transit supply=staged | rel=evac | ingest=1 capred=0",
			build: coAt(coSRTooling, "evac", S, "supply", S),
			act:   pairClick(dispNone)},

		// ── N-a′: the pickup chain releases a supply the pair click released ─
		{name: "N-a'/pickup chain, R2 still driving to its hold",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0",
			build: coAt(coPI3MarkedFront, "evac", S, "supply", S),
			act:   seq(pairClick(dispNone), picks("evac"))},
		{name: "N-a'/pickup chain, R2 already parked at its hold",
			want:  "ok | evac=in_transit supply=staged | rel=evac,supply | ingest=1 capred=0",
			build: coAt(coPI3MarkedFront, "evac", S, "supply", S),
			act:   seq(pairClick(dispNone), stages("supply"), picks("evac"))},
		// KEEP: the sweep defers the holdInbound supply to the evac's lift.
		{name: "keep/sweep defers the held supply to the evac's pickup",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | sweep released=2 pending=0 deferred=0 flip=[]",
			build: coAt(coPI3MarkedFront, "evac", S, "supply", S),
			act:   seq(sweepClick(dispNone), picks("evac"))},
		{name: "keep/two_robot changeover supply released at the evac's pickup",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | sweep released=2 pending=0 deferred=0 flip=[]",
			build: coAt(coTwoRobot, "evac", S, "supply", S),
			act:   seq(sweepClick(dispNone), picks("evac"))},

		// ── L1: no release goes for a wait nobody pressed ──────────────────
		{name: "L1/single_robot relay: stage leg confirms, swap leg at ready", gate: release.G2,
			want:  "ok | evac=staged supply=confirmed | rel=- | ingest=0 capred=0",
			build: coAt(coSRTooling, "evac", S, "supply", T),
			act:   confirms("supply")},
		{name: "L1/press-index tooling: R2 confirms, R1 at tooling done", gate: release.G2,
			want:  "ok | evac=staged supply=confirmed | rel=- | ingest=0 capred=0",
			build: coAt(coPITooling, "evac", S, "supply", T),
			act:   confirms("supply")},
		{name: "L1c/tooling done sweep, R2 parked at its hold",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply,supply,evac | ingest=1 capred=0 | sweep released=2 pending=0 deferred=0 flip=[]",
			build: coAt(coPI3MarkedFront, "evac", S, "supply", S),
			act:   seq(pairClick(dispNone), stages("evac"), stages("supply"), sweepClick(dispNone))},

		// ── The press-index pair (keep) ──────────────────────────────────────
		{name: "keep/pi changeover pair, both staged",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0",
			build: coAt(coPI, "evac", S, "supply", S),
			act:   pairClick(dispNone)},

		// ── Door 8: the intent worker ────────────────────────────────────────
		{name: "d8/two_robot supply stages after the click",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0",
			build: pairAt(twoRobot, "evac", S, "supply", D),
			act:   seq(pairClick(dispEmpty), stages("supply")), probe: probes(pDeferred)},
		{name: "d8/two_robot re-fire into a live curtain",
			want:  "ok | evac=in_transit supply=staged | rel=evac | ingest=1 capred=0 | deferred=supply | chip:supply=Release the light curtain at SYN-PRESS",
			build: withCurtain(curtainSafe, pairAt(twoRobot, "evac", S, "supply", D)),
			act: seq(pairClick(dispEmpty), func(h *relHarness) error { h.wl.set(curtainLive); return nil },
				stages("supply")),
			probe: probes(pDeferred, pChip("supply"))},
		{name: "L2/P-E1 refused, clicked again with the same qty",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac | ingest=0 capred=-5 | pile=5 | chip:evac=-",
			build: withBinAt(pairAt(twoRobotConsume, "evac", S, "supply", S)),
			act:   seq(pairClick(dispPull(5)), refuse("evac", "invalid_state"), pairClick(dispPull(5))),
			probe: probes(pPile, pChip("evac"))},
		{name: "L2/P-E2 refused, clicked again with a larger qty",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac | ingest=0 capred=-8 | pile=8",
			build: withBinAt(pairAt(twoRobotConsume, "evac", S, "supply", S)),
			act:   seq(pairClick(dispPull(5)), refuse("evac", "invalid_state"), pairClick(dispPull(8))),
			probe: probes(pPile)},
		{name: "L2/P-E3 refused, Edge restarts, clicked again",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac | ingest=0 capred=-5 | pile=5",
			build: withBinAt(pairAt(twoRobotConsume, "evac", S, "supply", S)),
			act:   seq(pairClick(dispPull(5)), refuse("evac", "invalid_state"), restarts(), pairClick(dispPull(5))),
			probe: probes(pPile)},
		{name: "L2/P-E4 refused on a gate wait, clicked again",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac | ingest=0 capred=-5 | pile=5",
			build: withBinAt(pairAt(twoRobotConsume, "evac", S, "supply", S)),
			act:   seq(pairClick(dispPull(5)), refuse("evac", "invalid_state"), pairClick(dispPull(5))),
			probe: probes(pPile)},
		{name: "L2/repeat with no refusal: per-order click on an in_transit evac",
			want:  "ok | evac=in_transit supply=dispatched | rel=evac,evac | ingest=0 capred=-5 | pile=5",
			build: withBinAt(pairAt(twoRobotConsume, "evac", S, "supply", D)),
			act:   seq(orderClick("evac", dispPull(5)), orderClick("evac", dispPull(5))),
			probe: probes(pPile)},
		{name: "L2/P-E5 refused Z with a deferred Y: one extra Y round trip, then nothing",
			want:  "ok | evac=staged supply=staged | rel=evac | ingest=0 capred=-5",
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
			want: "ok | evac=staged supply=dispatched | rel=evac | ingest=1 capred=0 | chip:evac=Core rejected the release", build: pairAt(twoRobot, "evac", S, "supply", D),
			act: seq(orderClick("evac", dispEmpty), refuse("evac", "invalid_state")), probe: probes(pChip("evac"))},
		{name: "N3/manifest_sync_failed",
			want: "ok | evac=staged supply=dispatched | rel=evac | ingest=1 capred=0 | chip:evac=Manifest sync failed at Core", build: pairAt(twoRobot, "evac", S, "supply", D),
			act: seq(orderClick("evac", dispEmpty), refuse("evac", "manifest_sync_failed")), probe: probes(pChip("evac"))},
		{name: "N3/fleet_failed",
			want: "ok | evac=failed supply=dispatched | rel=evac | ingest=1 capred=0 | chip:evac=-", build: pairAt(twoRobot, "evac", S, "supply", D),
			act: seq(orderClick("evac", dispEmpty), refuse("evac", "fleet_failed")), probe: probes(pChip("evac"))},
		{name: "N3/internal_error",
			want: "ok | evac=failed supply=dispatched | rel=evac | ingest=1 capred=0 | chip:evac=-", build: pairAt(twoRobot, "evac", S, "supply", D),
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
			want: "ok | nevac=in_transit nsupply=in_transit evac=submitted supply=submitted | rel=nevac,nsupply | ingest=1 capred=0",
			build: func(h *relHarness) {
				h.changeover(coSpec{mode: protocol.SwapModeTwoRobot, neighbour: true})
				statuses(h, "nevac", S, "nsupply", S)
			},
			act: func(h *relHarness) error { return h.eng.ReleaseStagedOrders(h.partnerID, dispEmpty) }},
		{name: "L4/per-order release of a departing produce leg",
			want:  "ok | evac=in_transit supply=dispatched | rel=evac | ingest=1 capred=0 | uop=0 | bin=nil",
			build: pairAt(twoRobot, "evac", S, "supply", D),
			act:   orderClick("evac", dispEmpty), probe: probes(pUOP, pBin)},
		{name: "L4/per-order release of the placing produce leg ships nothing",
			want:  "hold:lift | evac=in_transit supply=staged | rel=- | ingest=0 capred=0 | uop=42 | bin=9001",
			build: pairAt(twoRobot, "evac", T, "supply", S),
			act:   orderClick("supply", dispNone), probe: probes(pUOP, pBin)},

		// ── Door 3: the Material page ────────────────────────────────────────
		{name: "d3/material release",
			want:  "ok |  | rel=- | ingest=0 capred=0 | moves=1 | uop=42",
			build: func(h *relHarness) { h.seedNode(twoRobot) },
			act:   materialTap(0), probe: probes(pMoves, pUOP)},
		{name: "d3/material release tapped twice",
			want:  "refuse:in-flight |  | rel=- | ingest=0 capred=0 | moves=1",
			build: func(h *relHarness) { h.seedNode(twoRobot) },
			act:   seq(materialTap(0), materialTap(0)), probe: probes(pMoves)},
		{name: "d3/material release, curtain live",
			want:  "refuse:curtain |  | rel=- | ingest=0 capred=0 | moves=0",
			build: func(h *relHarness) { h.seedNode(twoRobot); h.armCurtain(curtainLive) },
			act:   materialTap(0), probe: probes(pMoves)},
		// ── Door 4: the changeover position evac (fallback claim) ────────────
		{name: "d4/position evac, curtain live",
			want:  "refuse:curtain |  | rel=- | ingest=0 capred=0 | moves=0",
			build: func(h *relHarness) { h.seedNode(twoRobot); h.armCurtain(curtainLive) },
			act:   positionEvac, probe: probes(pMoves)},

		// ── §15: a refused sequential release moves nothing ─────────────────
		// The flip is the first side effect and every refusal is above it: a
		// curtain refusal leaves the pull on the front and marks no
		// attribution boundary on the partner. The control cell is the same
		// release with the curtain clear.
		// S5: the curtain holds the release and remembers the press (Q8), and the
		// press still declares what it declares: the line moves to the partner
		// and the bin's count splits (the held press).
		{name: "§15/curtain holds a sequential release: the press flips the line and splits the count",
			want:  "hold:curtain | removal=staged | rel=- | ingest=1 capred=0 | pull=SYN-PRESS-B | boundary=1",
			build: seqWithCurtain(curtainLive),
			act:   orderClick("removal", dispEmpty), probe: probes(pPull, pBoundary)},
		{name: "§15/control: the same release with the curtain clear flips and marks the boundary",
			want:  "ok | removal=in_transit | rel=removal | ingest=1 capred=0 | pull=SYN-PRESS-B | boundary=1",
			build: seqWithCurtain(curtainSafe),
			act:   orderClick("removal", dispEmpty), probe: probes(pPull, pBoundary)},

		// ── L3: one press on a sequential A/B press ──────────────────────────
		// fact-owners Lane G (under this branch) already flips onto a ready
		// partner. The line always goes to the partner (owner, 2026-09-30), so a
		// partner that is not ready is the bug, and the sweep's decline is the
		// 2026-08-28 rule Lane G dropped.
		{name: "L3/per-order release at the pulled side, partner ready",
			want:  "ok | removal=in_transit | rel=removal | ingest=0 capred=0 | pull=SYN-PRESS-B",
			build: func(h *relHarness) { h.sequentialAB(protocol.ClaimRoleConsume, true); statuses(h, "removal", S) },
			act:   orderClick("removal", dispEmpty), probe: probes(pPull)},
		// The line always goes to the partner (owner, 2026-09-30): a partner
		// with no bin takes the line and its count waits for its bin.
		{name: "L3/per-order release at the pulled side, partner has no bin",
			want:  "ok | removal=in_transit | rel=removal | ingest=0 capred=0 | pull=SYN-PRESS-B",
			build: func(h *relHarness) { h.sequentialAB(protocol.ClaimRoleConsume, false); statuses(h, "removal", S) },
			act:   orderClick("removal", dispEmpty), probe: probes(pPull)},
		{name: "L3/changeover node click at the pulled side, partner ready",
			want: "ok | supply=in_transit | rel=supply | ingest=1 capred=0 | node released=1 pending=0 deferred=0 flip=[] | pull=SYN-PRESS-B",
			build: func(h *relHarness) {
				h.coID = h.changeover(coSpec{mode: protocol.SwapModeSequential})
				statuses(h, "supply", S)
				deliverBack(h)
			},
			act: nodeCOClick(dispNone), probe: probes(pPull)},
		// The partner is not ready and its outgoing carrier is still bound. The
		// rule says flip and hold the count for the incoming carrier, but the
		// tick path charges whatever bin is bound, so hold-and-replay cannot
		// keep the count off the outgoing one. Refused as before; reported to
		// the owner.
		{name: "L3/changeover node click at the pulled side, partner holds its outgoing carrier",
			bug:   "L3-co",
			today: "refuse:outgoing-carrier | supply=staged | rel=- | ingest=0 capred=0 | node released=0 pending=0 deferred=0 flip=[] | pull=SYN-PRESS",
			want:  "ok | supply=in_transit | rel=supply | ingest=1 capred=0 | node released=1 pending=0 deferred=0 flip=[] | pull=SYN-PRESS-B",
			build: func(h *relHarness) {
				h.coID = h.changeover(coSpec{mode: protocol.SwapModeSequential})
				statuses(h, "supply", S)
			},
			act: nodeCOClick(dispNone), probe: probes(pPull)},
		{name: "L3/sweep declines the pulled side",
			want: "ok | supply=staged | rel=- | ingest=0 capred=0 | sweep released=0 pending=2 deferred=0 flip=[SYN-PRESS] | pull=SYN-PRESS",
			build: func(h *relHarness) {
				h.coID = h.changeover(coSpec{mode: protocol.SwapModeSequential})
				statuses(h, "supply", S)
				deliverBack(h)
			},
			act: sweepClick(dispNone), probe: probes(pPull)},
	}
}
