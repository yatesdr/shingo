// line_counter_test.go — a station counter's strokes charge the positions
// counted at it, under the style the PLC's part attributes; the production
// walk leaves those positions to it.
package engine

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/plc"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// stationCounter creates a station counter on the process and binds nodes to it.
func stationCounter(t *testing.T, db *store.DB, pid int64, nodes ...int64) int64 {
	t.Helper()
	id, err := db.CreateLineCounter(pid, "load station", "plc", "MES_Cell.Station_Counter_01", "")
	testutil.MustNoErr(t, err, "station counter")
	for _, n := range nodes {
		testutil.MustNoErr(t, db.SetNodeLineCounter(n, &id), "bind")
	}
	return id
}

// stationTick emits one station counter's strokes as the poll does.
func stationTick(eng *Engine, counterID, pid int64, id plc.TickIdentity, delta int64) {
	em := &plcEmitter{bus: eng.Events, eng: eng}
	em.EmitLineCounterDelta(counterID, pid, delta, "", id)
}

// TestLineTick_IncomingOnlyNodeChargedOnceThePLCRunsTheIncomingPart: a position
// only the incoming style claims, counted at a station: a stroke while the PLC
// runs today's part is not its (today's style does not claim it); once the PLC
// runs the target's part it is charged.
func TestLineTick_IncomingOnlyNodeChargedOnceThePLCRunsTheIncomingPart(t *testing.T) {
	t.Parallel()
	eng, pid, a, b, _ := attributeRig(t, "1002", "1001")
	eng.wireEventHandlers()
	nodeID, err := eng.db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: "TO-ONLY", Code: "TO-ONLY", Name: "TO-ONLY", Enabled: true})
	testutil.MustNoErr(t, err, "node")
	claimID, err := upsertClaimRetiredMode(eng.db, processes.NodeClaimInput{StyleID: b, CoreNodeName: "TO-ONLY", Role: "consume",
		SwapMode: "simple", PayloadCode: "PART-NEW", UOPCapacity: 100, InboundSource: "SRC"})
	testutil.MustNoErr(t, err, "claim")
	_, err = eng.db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, binPtr(9411), 40), "a cart")
	_, err = eng.changeoverService.Create(pid, &a, b, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "open a changeover to B")
	lc := stationCounter(t, eng.db, pid, nodeID)

	stationTick(eng, lc, pid, plc.TickIdentity{StyleID: a, CATID: "1001", Kind: plc.TickToday}, 2)
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 40 {
		t.Errorf("after a stroke on today's part = %d, want 40", got)
	}
	em := &plcEmitter{bus: eng.Events, eng: eng}
	id := em.AttributeTick(pid, a)
	if id.Kind != plc.TickTarget {
		t.Fatalf("attribution = %+v, want the target", id)
	}
	stationTick(eng, lc, pid, id, 3)
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 37 {
		t.Errorf("after a stroke on the incoming part = %d, want 37", got)
	}
}

// TestStation_NoIdentityKeepsTheActiveStyle: with no part read (no style in
// the attribution: no monitor, identity none, a stale read), or today's part, a
// station stroke is charged under the active style; it opens no hold. (An
// off-plan or held stroke holds its share: station_holds_test.go.)
func TestStation_NoIdentityKeepsTheActiveStyle(t *testing.T) {
	t.Parallel()
	r := newPairRig(t)
	n := r.consume("ST-1", "", "simple", true)
	lc := stationCounter(t, r.db, r.pid, n)
	for i, id := range []plc.TickIdentity{
		{},
		{StyleID: r.style, Kind: plc.TickToday},
		{StyleID: r.style, CATID: "1001", Kind: plc.TickToday},
	} {
		stationTick(r.eng, lc, r.pid, id, 1)
		if got := r.left(n); got != 49-i {
			t.Errorf("stroke %d (%v) = %d, want %d", i, id.Kind, got, 49-i)
		}
	}
	hs, err := r.db.ListOpenCatidHolds(r.pid)
	testutil.MustNoErr(t, err, "holds")
	if len(hs) != 0 {
		t.Errorf("holds = %+v, want none from a station", hs)
	}
}

// TestProductionTick_LeavesABoundPositionToItsCounter: the production walk
// does not charge a position counted at a station, and counts it as landed.
// A dangling binding (its counter gone) reads as unbound and is charged.
func TestProductionTick_LeavesABoundPositionToItsCounter(t *testing.T) {
	t.Parallel()
	r := newPairRig(t)
	bound := r.consume("PB-1", "", "simple", true)
	free := r.consume("PB-2", "", "simple", true)
	dangling := r.consume("PB-3", "", "simple", true)
	stationCounter(t, r.db, r.pid, bound)
	gone := int64(4242)
	testutil.MustNoErr(t, r.db.SetNodeLineCounter(dangling, &gone), "a binding to no counter")
	r.tick(2)
	if got := []int{r.left(bound), r.left(free), r.left(dangling)}; !(got[0] == 50 && got[1] == 48 && got[2] == 48) {
		t.Errorf("carts = %v, want [50 48 48]: the bound one is its station's", got)
	}
	if (tickAttribution{otherCounter: 1}).landed() != true {
		t.Error("a position left to its counter is not landed: an all-bound process would log every stroke unattributed")
	}
}

// TestLineTick_BoundNodeTurnedPairedCountsAsUnbound: a bound position whose
// claim became an A/B pair is counted by the production walk, not the station.
func TestLineTick_BoundNodeTurnedPairedCountsAsUnbound(t *testing.T) {
	t.Parallel()
	r := newPairRig(t)
	a := r.consume("TP-A", "TP-B", "simple", true)
	b := r.consume("TP-B", "TP-A", "simple", false)
	lc := stationCounter(t, r.db, r.pid, a, b)
	stationTick(r.eng, lc, r.pid, plc.TickIdentity{StyleID: r.style, Kind: plc.TickToday}, 3)
	if got := []int{r.left(a), r.left(b)}; !(got[0] == 50 && got[1] == 50) {
		t.Errorf("after a station stroke = %v, want [50 50]: a pair is not the station's", got)
	}
	r.tick(2)
	if got := r.left(a); got != 48 {
		t.Errorf("after a production stroke = %d, want 48", got)
	}
}

// TestLineTick_GapTickIsLogged: through the station path, station strokes in
// the gap between the outgoing cart's lift and the incoming cart's landing are
// logged at the position (decision 55), as the production walk logs them.
func TestLineTick_GapTickIsLogged(t *testing.T) {
	t.Parallel()
	eng, db, pid, nodeID, from, _ := runOutRig(t)
	lc := stationCounter(t, db, pid, nodeID)
	today := plc.TickIdentity{StyleID: from, Kind: plc.TickToday}
	stationTick(eng, lc, pid, today, 2)
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 15 {
		t.Fatalf("old cart at the lift = %d, want 15", got)
	}
	testutil.MustNoErr(t, eng.carrierLeft(nodeID, runOutNode), "the lift")
	stationTick(eng, lc, pid, today, 3)
	if s := readSeat55(t, eng, pid, nodeID); s.hold != 0 || s.logged != 3 {
		t.Errorf("seat after the gap = hold %d, logged %d; want no hold, 3 logged", s.hold, s.logged)
	}
}

// TestStation_PileDrainsUnderTheTicksStyle — named behaviour: a station stroke
// drains the lineside pile of its style's part at the position before the cart.
// While the PLC runs today's part, the outgoing part's pile drains first; once
// it runs the incoming part, the stroke is the target's and the outgoing pile
// is left (it strands at the cutover), as is the outgoing cart: the stroke is
// logged (decision 55).
func TestStation_PileDrainsUnderTheTicksStyle(t *testing.T) {
	t.Parallel()
	eng, db, pid, nodeID, from, to := runOutRig(t)
	lc := stationCounter(t, db, pid, nodeID)
	_, err := db.CaptureLinesideBucket(nodeID, "PART-OLD", 4)
	testutil.MustNoErr(t, err, "a pile of the outgoing part")
	stationTick(eng, lc, pid, plc.TickIdentity{StyleID: from, Kind: plc.TickToday}, 3)
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 17 {
		t.Errorf("cart after 3 on today's part = %d, want 17: the pile pays first", got)
	}
	stationTick(eng, lc, pid, plc.TickIdentity{StyleID: to, Kind: plc.TickTarget}, 2)
	if s := readSeat55(t, eng, pid, nodeID); s.cart != 17 || s.hold != 0 || s.logged != 2 {
		t.Errorf("cart after 2 on the incoming part = %d, hold %d, logged %d; want 17, none and 2 logged: the outgoing "+
			"pile is not the target's, nor is the outgoing cart", s.cart, s.hold, s.logged)
	}
	buckets, err := db.ListLinesideBuckets(nodeID)
	testutil.MustNoErr(t, err, "piles")
	left := 0
	for _, b := range buckets {
		if b.PayloadCode == "PART-OLD" && b.State == "active" {
			left += b.Qty
		}
	}
	if left != 1 {
		t.Errorf("outgoing pile = %d, want 1 (4 less the 3 today's strokes drew)", left)
	}
}

// TestStationStroke_NeverPokesTheCutover: a station stroke on the incoming
// part, attributed to the target, never reaches the counted-part force: the
// station emit records no counted tick and pokes nothing, so the changeover
// stays open behind its red gate. Only the production counter's stroke forces
// (TestForcedCutover_CountedIncomingPartCompletesPastARedGate).
func TestStationStroke_NeverPokesTheCutover(t *testing.T) {
	t.Parallel()
	eng, db, pid, from, coID, _, cm := forceRig(t, "auto")
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	settle(cm, pid, st, "1002", time.Now())
	lc := stationCounter(t, db, pid)
	em := &plcEmitter{bus: eng.Events, eng: eng}
	id := em.AttributeTick(pid, from)
	if id.Kind != plc.TickTarget {
		t.Fatalf("attribution = %+v, want the target's", id)
	}
	em.EmitLineCounterDelta(lc, pid, 1, "", id)
	if n := drainPokes(eng, cm); n != 0 {
		t.Errorf("a station stroke poked the cutover %d time(s)", n)
	}
	if got := cm.lastCountedTick(pid); got != (countedTick{}) {
		t.Errorf("a station stroke was recorded as the counted tick: %+v", got)
	}
	tickAutoCutover(cm, pid, st, "1002", true, time.Now().Add(61*time.Second))
	if co := changeoverRow(t, db, coID); co.State.IsTerminal() {
		t.Errorf("a station stroke forced the changeover: %s by %q", co.State, co.TriggeredBy)
	}
}
