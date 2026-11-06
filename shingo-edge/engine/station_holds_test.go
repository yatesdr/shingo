// station_holds_test.go — a station stroke the PLC made under a part Shingo
// cannot place by its style holds its draw, as the cell counter's does; and a
// hold at a station-counted position is charged and released at that position.
package engine

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/plc"
	"shingoedge/store/processes"
)

// alsoClaim adds style's consume claim of part at an existing position.
func alsoClaim(t *testing.T, eng *Engine, style int64, core, part string) {
	t.Helper()
	_, err := upsertClaimRetiredMode(eng.db, processes.NodeClaimInput{StyleID: style, CoreNodeName: core, Role: "consume",
		SwapMode: "simple", PayloadCode: part, UOPCapacity: 100, InboundSource: "SRC"})
	testutil.MustNoErr(t, err, "claim "+core)
}

// TestStation_OffPlanStrokeHoldsItsShare: the PLC runs a third style's pinned
// part (the running style's set proven). At a station-counted position whose
// cart is the running style's part, the station stroke draws nothing and holds
// one off-plan share of that style's part there; the cell counter's stroke for
// the same assembly leaves the position to its station. Before, the station
// stroke was folded back to the running style and drew its cart.
func TestStation_OffPlanStrokeHoldsItsShare(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := attributeRig(t, "1003", "1001")
	eng.wireEventHandlers()
	s := pinnedStyle(t, eng, pid, "STYLE-S", "1003")
	x := cartNode(t, eng, pid, a, "SX", "PART-A", "PART-A", 9951)
	alsoClaim(t, eng, s, "SX", "PART-S")
	lc := stationCounter(t, eng.db, pid, x)

	em := &plcEmitter{bus: eng.Events, eng: eng}
	id := em.AttributeTick(pid, a)
	if id.Kind != plc.TickOffPlan || id.StyleID != s {
		t.Fatalf("attribution = %+v, want off-plan on S", id)
	}
	stationTick(eng, lc, pid, id, 2)
	if got := runtimeOf(t, eng, x).RemainingUOPCached; got != 40 {
		t.Errorf("the running style's cart after an off-plan station stroke = %d, want 40", got)
	}
	strokeAt(eng, pid, a, 2) // the cell counter's stroke for the same assembly
	hs := openHolds(t, eng, pid)
	if len(hs) != 1 || hs[0].ProcessNodeID != x || hs[0].PayloadCode != "PART-S" || hs[0].Reason != holdReasonOffPlan || hs[0].Qty != 2 {
		t.Fatalf("holds = %+v, want one off-plan share of 2 PART-S at SX", hs)
	}
	if got := runtimeOf(t, eng, x).RemainingUOPCached; got != 40 {
		t.Errorf("cart after the cell stroke = %d, want 40: the position is its station's", got)
	}
}

// TestStation_HeldStrokeHoldsPerNode: the PLC runs a part no style runs (the
// running style's set proven). The station stroke holds one share per position
// counted at it, no part named, and draws no cart. Before, it drew both carts.
func TestStation_HeldStrokeHoldsPerNode(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := attributeRig(t, "1007", "1001")
	eng.wireEventHandlers()
	x := cartNode(t, eng, pid, a, "HX", "PART-A", "PART-A", 9961)
	y := cartNode(t, eng, pid, a, "HY", "PART-A", "PART-A", 9962)
	lc := stationCounter(t, eng.db, pid, x, y)

	em := &plcEmitter{bus: eng.Events, eng: eng}
	id := em.AttributeTick(pid, a)
	if id.Kind != plc.TickHeld {
		t.Fatalf("attribution = %+v, want held", id)
	}
	stationTick(eng, lc, pid, id, 3)
	for _, n := range []int64{x, y} {
		if got := runtimeOf(t, eng, n).RemainingUOPCached; got != 40 {
			t.Errorf("cart at node %d = %d, want 40", n, got)
		}
	}
	hs := openHolds(t, eng, pid)
	if len(hs) != 2 {
		t.Fatalf("holds = %+v, want one per position", hs)
	}
	for i, n := range []int64{x, y} {
		h := hs[i]
		if h.ProcessNodeID != n || h.PayloadCode != "" || h.Reason != holdReasonHeld || h.Qty != 3 || h.CATID != "1007" {
			t.Errorf("hold %d = %+v, want node %d's held share of 3, no part", i, h, n)
		}
	}
}

// TestStation_StrokeNeverWritesTheProductionWholeDraw: a held station stroke
// writes no node-0 row — that row is the cell counter's whole draw, whose
// release walks the cell counter's positions, not the station's.
func TestStation_StrokeNeverWritesTheProductionWholeDraw(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := attributeRig(t, "1007", "1001")
	eng.wireEventHandlers()
	x := cartNode(t, eng, pid, a, "WX", "PART-A", "PART-A", 9963)
	lc := stationCounter(t, eng.db, pid, x)
	stationTick(eng, lc, pid, plc.TickIdentity{StyleID: a, CATID: "1007", Kind: plc.TickHeld}, 2)
	for _, h := range openHolds(t, eng, pid) {
		if h.ProcessNodeID == 0 {
			t.Errorf("a station stroke wrote the cell counter's whole-draw row: %+v", h)
		}
	}
}

// TestHold_ChargeReachesAStationCountedPosition: a share held at a
// station-counted position is charged to its cart. Before, the Charge walked it
// as the cell counter's, skipped the position, and resolved the row with
// nothing drawn.
func TestHold_ChargeReachesAStationCountedPosition(t *testing.T) {
	t.Parallel()
	eng, pid, _, _, _ := holdRig(t, "1003")
	s := pinnedStyle(t, eng, pid, "STYLE-S", "1003")
	x := cartNode(t, eng, pid, s, "CX", "PART-S", "PART-S", 9971)
	stationCounter(t, eng.db, pid, x)
	_, err := eng.db.HoldCatidDraw(pid, "1003", x, "PART-S", holdReasonOffPlan, 2, time.Now())
	testutil.MustNoErr(t, err, "a station share")
	id := mustOneHold(t, eng.db, pid).ID
	p, err := eng.PreviewCatidHold(id)
	testutil.MustNoErr(t, err, "preview")
	if p.Refusal != "" || len(p.Rows) != 1 || p.Rows[0].Node != "CX" || p.Rows[0].Qty != 2 {
		t.Fatalf("preview = %+v, want the CX cart, 2", p)
	}
	testutil.MustNoErr(t, eng.ChargeCatidHold(id, "engineer"), "charge")
	if got := runtimeOf(t, eng, x).RemainingUOPCached; got != 38 {
		t.Errorf("CX's cart after the Charge = %d, want 38", got)
	}
	h, err := eng.db.GetCatidHold(id)
	testutil.MustNoErr(t, err, "hold")
	if h.Resolution != holdCharged {
		t.Errorf("hold = %+v, want charged", h)
	}
}

// TestHold_LandingReleasesAStationShare: a cutover landing on the style that
// runs the CATID releases a station's held share through the same walk.
func TestHold_LandingReleasesAStationShare(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := holdRig(t, "1003")
	s := pinnedStyle(t, eng, pid, "STYLE-S", "1003")
	x := cartNode(t, eng, pid, s, "LX", "PART-S", "PART-S", 9981)
	stationCounter(t, eng.db, pid, x)
	_, err := eng.db.HoldCatidDraw(pid, "1003", x, "", holdReasonHeld, 2, time.Now().Add(-time.Minute))
	testutil.MustNoErr(t, err, "a station's held share")
	id := mustOneHold(t, eng.db, pid).ID
	landOn(t, eng, pid, a, s)
	if got := runtimeOf(t, eng, x).RemainingUOPCached; got != 38 {
		t.Errorf("LX's cart after the landing = %d, want 38", got)
	}
	h, err := eng.db.GetCatidHold(id)
	testutil.MustNoErr(t, err, "hold")
	if h.Resolution != holdReleased {
		t.Errorf("hold = %+v, want released", h)
	}
}
