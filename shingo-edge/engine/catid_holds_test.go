// catid_holds_test.go — a counted stroke Shingo cannot place by the PLC's
// part has its cart draw held in a ledger; its production record still ships.
package engine

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/plc"
	"shingoedge/store"
	"shingoedge/store/counters"
	"shingoedge/store/processes"
	"shingoedge/uop"
)

// holdRig is attributeRig with A proven (1001 observed), the event handlers
// wired, and the produce node's seat empty (no cart) — a draw there would be
// the seat hold.
func holdRig(t *testing.T, live string) (eng *Engine, pid, a, b, produceNode int64) {
	t.Helper()
	eng, pid, a, b, _ = attributeRig(t, live, "1001")
	eng.wireEventHandlers()
	nodes, err := eng.db.ListProcessNodesByProcess(pid)
	testutil.MustNoErr(t, err, "nodes")
	return eng, pid, a, b, nodes[0].ID
}

// strokeAt emits one counted delta the way the poll does: attributed first.
func strokeAt(eng *Engine, pid, pointStyle int64, delta int64) plc.TickIdentity {
	em := &plcEmitter{bus: eng.Events, eng: eng}
	id := em.AttributeTick(pid, pointStyle)
	em.EmitCounterDelta(0, pid, delta, delta, "", id)
	return id
}

// cartNode adds a consume node the style claims for part, with a cart of
// cartPart (known) holding 40.
func cartNode(t *testing.T, eng *Engine, pid, styleID int64, name, part, cartPart string, bin int64) int64 {
	t.Helper()
	nodeID, err := eng.db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: name, Code: name, Name: name, Enabled: true})
	testutil.MustNoErr(t, err, "node "+name)
	claimID, err := upsertClaimRetiredMode(eng.db, processes.NodeClaimInput{StyleID: styleID, CoreNodeName: name, Role: "consume",
		SwapMode: "simple", PayloadCode: part, UOPCapacity: 100, InboundSource: "SRC"})
	testutil.MustNoErr(t, err, "claim "+name)
	_, err = eng.db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime "+name)
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, binPtr(bin), 40), "cart "+name)
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeLinesidePayload(nodeID, cartPart, true, "test"), "cart part "+name)
	return nodeID
}

// pinnedStyle creates a style whose set is catid.
func pinnedStyle(t *testing.T, eng *Engine, pid int64, name, catid string) int64 {
	t.Helper()
	id, err := eng.db.CreateStyle(name, "", pid)
	testutil.MustNoErr(t, err, "style "+name)
	testutil.MustNoErr(t, eng.db.SetStyleExpectedCATID(id, catid), "pin "+name)
	return id
}

func openHolds(t *testing.T, eng *Engine, pid int64) []store.CatidHold {
	t.Helper()
	hs, err := eng.db.ListOpenCatidHolds(pid)
	testutil.MustNoErr(t, err, "open holds")
	return hs
}

func hourlyUnder(t *testing.T, eng *Engine, pid, styleID int64) int64 {
	t.Helper()
	bucket := counters.HourBucket(time.Now())
	rows, err := eng.db.ListHourlyCounts(pid, styleID, bucket-3600, bucket+3600)
	testutil.MustNoErr(t, err, "hourly")
	n := int64(0)
	for _, r := range rows {
		n += r.Delta
	}
	return n
}

// landOn lands the process on style to through the door, forced (the rig's
// produce node would gate it).
func landOn(t *testing.T, eng *Engine, pid, from, to int64) int64 {
	t.Helper()
	coID, err := eng.changeoverService.Create(pid, &from, to, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "open a changeover")
	_, err = eng.completeCutover(pid, coID, triggeredManual, CutoverForced)
	testutil.MustNoErr(t, err, "land it")
	return coID
}

// TestHold_HeldTickStillShipsUnderTodaysStyle: a stroke on a CATID no style
// runs counts the hour under the running style, draws no cart and writes no
// seat hold; its draw opens a hold, grown stroke by stroke. Before, the walk
// charged it to the running style's seat.
func TestHold_HeldTickStillShipsUnderTodaysStyle(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, pn := holdRig(t, "1007")
	if id := strokeAt(eng, pid, a, 3); id.Kind != plc.TickHeld || id.StyleID != a {
		t.Fatalf("attribution = %+v, want held on A", id)
	}
	strokeAt(eng, pid, a, 1)
	if n := hourlyUnder(t, eng, pid, a); n != 4 {
		t.Errorf("hourly under A = %d, want 4: the record ships", n)
	}
	if rt := runtimeOf(t, eng, pn); rt.PendingUOPDelta != 0 || rt.RemainingUOPCached != 50 {
		t.Errorf("seat after held strokes: pending %d, count %d, want 0 and 50", rt.PendingUOPDelta, rt.RemainingUOPCached)
	}
	hs := openHolds(t, eng, pid)
	if len(hs) != 1 || hs[0].CATID != "1007" || hs[0].ProcessNodeID != 0 || hs[0].Qty != 4 || hs[0].Strokes != 2 || hs[0].Reason != holdReasonHeld {
		t.Fatalf("holds = %+v, want one held draw of 4 over 2 strokes on 1007", hs)
	}
	if it, ok := healthKinds(t, eng, pid)[catidKindHeld]; !ok || it.Severity != CatidHealthRed {
		t.Errorf("health: %+v, want a red held item", it)
	}
}

// TestHold_OffPlanChargesMatchingCartsHoldsTheRest: a stroke on one other
// style's part charges that style's carts of its part and holds its share at
// the nodes whose cart is another part.
func TestHold_OffPlanChargesMatchingCartsHoldsTheRest(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := holdRig(t, "1003")
	s := pinnedStyle(t, eng, pid, "STYLE-S", "1003")
	var same, other []int64
	for i := 0; i < 3; i++ {
		same = append(same, cartNode(t, eng, pid, s, fmt.Sprintf("S-SAME-%d", i), "PART-S", "PART-S", int64(9501+i)))
	}
	for i := 0; i < 2; i++ {
		other = append(other, cartNode(t, eng, pid, s, fmt.Sprintf("S-OTHER-%d", i), "PART-S", "PART-A", int64(9601+i)))
	}
	if id := strokeAt(eng, pid, a, 2); id.Kind != plc.TickOffPlan || id.StyleID != s {
		t.Fatalf("attribution = %+v, want off-plan on S", id)
	}
	for _, n := range same {
		if got := runtimeOf(t, eng, n).RemainingUOPCached; got != 38 {
			t.Errorf("S's cart of its part at node %d = %d, want 38", n, got)
		}
	}
	for _, n := range other {
		if got := runtimeOf(t, eng, n).RemainingUOPCached; got != 40 {
			t.Errorf("the other part's cart at node %d = %d, want 40 (held)", n, got)
		}
	}
	hs := openHolds(t, eng, pid)
	if len(hs) != 2 {
		t.Fatalf("holds = %+v, want the two other-part nodes' shares", hs)
	}
	for i, h := range hs {
		if h.ProcessNodeID != other[i] || h.PayloadCode != "PART-S" || h.Qty != 2 || h.Reason != holdReasonOffPlan {
			t.Errorf("hold %d = %+v, want node %d's share of 2 PART-S", i, h, other[i])
		}
	}
	if n := hourlyUnder(t, eng, pid, s); n != 2 {
		t.Errorf("hourly under S = %d, want 2", n)
	}
}

// TestHold_UnprovenActiveSetNeverHolds: with the running style's set never
// seen on the PLC, a stroke on another style's part is today's, and nothing
// is held.
func TestHold_UnprovenActiveSetNeverHolds(t *testing.T) {
	t.Parallel()
	eng, _, pid, a, _, cm := healthRig(t, "1003")
	eng.tickMonitor.Store(cm)
	eng.wireEventHandlers()
	s := pinnedStyle(t, eng, pid, "STYLE-S", "1003")
	cartNode(t, eng, pid, s, "S-OTHER", "PART-S", "PART-A", 9701)
	if id := strokeAt(eng, pid, a, 1); id.Kind != plc.TickToday || id.StyleID != a {
		t.Fatalf("attribution = %+v, want today on A", id)
	}
	if hs := openHolds(t, eng, pid); len(hs) != 0 {
		t.Errorf("holds = %+v, want none", hs)
	}
	// And the 'no part' value: a -1 stroke is today's, never held.
	setLive(cm, pid, "-1")
	if id := strokeAt(eng, pid, a, 1); id.Kind != plc.TickToday {
		t.Errorf("-1 attribution = %+v, want today", id)
	}
	if hs := openHolds(t, eng, pid); len(hs) != 0 {
		t.Errorf("holds after -1 = %+v, want none", hs)
	}
}

// TestHold_NeverWritesTheSeatHold: neither a held stroke nor an off-plan
// share held at a node writes the node's seat hold (pending_uop_*), which is
// one slot for one cart's absent carrier. Before, a held stroke walked under
// the running style into the empty seat's pending.
func TestHold_NeverWritesTheSeatHold(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, pn := holdRig(t, "1007")
	strokeAt(eng, pid, a, 2)
	s := pinnedStyle(t, eng, pid, "STYLE-S", "1003")
	empty, err := eng.db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: "S-EMPTY", Code: "S-EMPTY", Name: "S-EMPTY", Enabled: true})
	testutil.MustNoErr(t, err, "node")
	claimID, err := upsertClaimRetiredMode(eng.db, processes.NodeClaimInput{StyleID: s, CoreNodeName: "S-EMPTY", Role: "consume",
		SwapMode: "simple", PayloadCode: "PART-S", UOPCapacity: 100, InboundSource: "SRC"})
	testutil.MustNoErr(t, err, "claim")
	_, err = eng.db.EnsureProcessNodeRuntime(empty)
	testutil.MustNoErr(t, err, "runtime")
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeWithBin(empty, &claimID, nil, 0), "an empty seat")
	setLive(eng.catidMon, pid, "1003")
	eng.catidMon.snapshots.Store(pid, &catidSnapshot{value: "1003", lastOK: time.Now(), observed: set("1001")})
	if id := strokeAt(eng, pid, a, 3); id.Kind != plc.TickOffPlan {
		t.Fatalf("attribution = %+v, want off-plan", id)
	}
	for _, n := range []int64{pn, empty} {
		if rt := runtimeOf(t, eng, n); rt.PendingUOPDelta != 0 || rt.PendingUOPPayload != "" {
			t.Errorf("node %d seat hold = %d %q, want none", n, rt.PendingUOPDelta, rt.PendingUOPPayload)
		}
	}
	if hs := openHolds(t, eng, pid); len(hs) != 2 || hs[1].ProcessNodeID != empty || hs[1].Qty != 3 {
		t.Errorf("holds = %+v, want the held draw and the empty seat's off-plan share in the ledger", hs)
	}
}

// TestHold_ReleasesOnALandingOnItsStyle: a held draw is charged when a
// changeover lands the process on the style whose set holds its CATID, with
// no other changeover started since it opened — once, under that style.
func TestHold_ReleasesOnALandingOnItsStyle(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := holdRig(t, "1003")
	strokeAt(eng, pid, a, 2)
	c := pinnedStyle(t, eng, pid, "STYLE-C", "1003")
	cNode := cartNode(t, eng, pid, c, "C-NODE", "PART-C", "PART-C", 9801)
	before := hourlyUnder(t, eng, pid, a) + hourlyUnder(t, eng, pid, c)
	hs := openHolds(t, eng, pid)
	if len(hs) != 1 {
		t.Fatalf("holds = %+v, want one: 1003 ran no style when it was counted", hs)
	}

	landOn(t, eng, pid, a, c)
	if got := runtimeOf(t, eng, cNode).RemainingUOPCached; got != 38 {
		t.Errorf("C's cart after the landing = %d, want 38: the held draw charged under C", got)
	}
	h, err := eng.db.GetCatidHold(hs[0].ID)
	testutil.MustNoErr(t, err, "hold")
	if h.ResolvedAt == nil || h.Resolution != holdReleased || h.ResolvedBy != "auto" {
		t.Errorf("hold = %+v, want released by auto", h)
	}
	if after := hourlyUnder(t, eng, pid, a) + hourlyUnder(t, eng, pid, c); after != before {
		t.Errorf("hourly %d -> %d: the release counted the hour again", before, after)
	}
	if _, ok := healthKinds(t, eng, pid)[catidKindHeld]; ok {
		t.Error("the held item outlived its release")
	}
}

// TestHold_ReleasesOnAnAdminSwitchToItsStyle: an engineer's switch is a
// landing too.
func TestHold_ReleasesOnAnAdminSwitchToItsStyle(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := holdRig(t, "1003")
	strokeAt(eng, pid, a, 2)
	c := pinnedStyle(t, eng, pid, "STYLE-C", "1003")
	cNode := cartNode(t, eng, pid, c, "C-NODE", "PART-C", "PART-C", 9802)
	testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &c), "switch to C")
	if got := runtimeOf(t, eng, cNode).RemainingUOPCached; got != 38 {
		t.Errorf("C's cart after the switch = %d, want 38", got)
	}
	if hs := openHolds(t, eng, pid); len(hs) != 0 {
		t.Errorf("holds = %+v, want none", hs)
	}
}

// TestHold_ChangeoverStartedSinceHoldWaitsForAPerson: a changeover started
// after the hold opened means the plant moved on; the later landing on the
// CATID's style does not charge it.
func TestHold_ChangeoverStartedSinceHoldWaitsForAPerson(t *testing.T) {
	t.Parallel()
	eng, pid, a, b, _ := holdRig(t, "1003")
	strokeAt(eng, pid, a, 2)
	landOn(t, eng, pid, a, b)
	c := pinnedStyle(t, eng, pid, "STYLE-C", "1003")
	cNode := cartNode(t, eng, pid, c, "C-NODE", "PART-C", "PART-C", 9803)
	landOn(t, eng, pid, b, c)
	if got := runtimeOf(t, eng, cNode).RemainingUOPCached; got != 40 {
		t.Errorf("C's cart = %d, want 40: the hold waits for a person", got)
	}
	if hs := openHolds(t, eng, pid); len(hs) != 1 {
		t.Errorf("holds = %+v, want the one still open", hs)
	}
}

// TestHold_ChargeLatchesOnce: a person's Charge charges under the CATID's one
// style, once; a second Charge or a Write off after it is refused and moves
// nothing.
func TestHold_ChargeLatchesOnce(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := holdRig(t, "1003")
	strokeAt(eng, pid, a, 2)
	c := pinnedStyle(t, eng, pid, "STYLE-C", "1003")
	cNode := cartNode(t, eng, pid, c, "C-NODE", "PART-C", "PART-C", 9804)
	id := mustOneHold(t, eng.db, pid).ID
	p, err := eng.PreviewCatidHold(id)
	testutil.MustNoErr(t, err, "preview")
	if p.Refusal != "" || p.StyleID != c || len(p.Rows) != 1 || p.Rows[0].BinID != 9804 || p.Rows[0].Qty != 2 {
		t.Fatalf("preview = %+v, want C, the one cart, 2", p)
	}
	testutil.MustNoErr(t, eng.ChargeCatidHold(id, "engineer"), "charge")
	if err := eng.ChargeCatidHold(id, "engineer"); !errors.Is(err, ErrHoldResolved) {
		t.Errorf("second charge = %v, want ErrHoldResolved", err)
	}
	if err := eng.WriteOffCatidHold(id, "engineer"); !errors.Is(err, ErrHoldResolved) {
		t.Errorf("write-off after a charge = %v, want ErrHoldResolved", err)
	}
	if got := runtimeOf(t, eng, cNode).RemainingUOPCached; got != 38 {
		t.Errorf("C's cart = %d, want 38: charged once", got)
	}
	h, err := eng.db.GetCatidHold(id)
	testutil.MustNoErr(t, err, "hold")
	if h.Resolution != holdCharged || h.ResolvedBy != "engineer" {
		t.Errorf("hold = %+v, want charged by engineer", h)
	}
}

// TestHold_WriteOffChargesNothing: the Write off closes the hold and draws
// from no cart.
func TestHold_WriteOffChargesNothing(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := holdRig(t, "1003")
	strokeAt(eng, pid, a, 2)
	c := pinnedStyle(t, eng, pid, "STYLE-C", "1003")
	cNode := cartNode(t, eng, pid, c, "C-NODE", "PART-C", "PART-C", 9805)
	id := mustOneHold(t, eng.db, pid).ID
	testutil.MustNoErr(t, eng.WriteOffCatidHold(id, "engineer"), "write off")
	if got := runtimeOf(t, eng, cNode).RemainingUOPCached; got != 40 {
		t.Errorf("C's cart = %d, want 40", got)
	}
	h, err := eng.db.GetCatidHold(id)
	testutil.MustNoErr(t, err, "hold")
	if h == nil || h.Resolution != holdWrittenOff {
		t.Errorf("hold = %+v, want written off", h)
	}
}

// TestHold_ChargeRefusedOntoAWrongPartCarrier: a Charge never draws a cart of
// another part; the preview names it and the hold stays open.
func TestHold_ChargeRefusedOntoAWrongPartCarrier(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := holdRig(t, "1003")
	s := pinnedStyle(t, eng, pid, "STYLE-S", "1003")
	n := cartNode(t, eng, pid, s, "S-OTHER", "PART-S", "PART-A", 9901)
	strokeAt(eng, pid, a, 2)
	hs := openHolds(t, eng, pid)
	if len(hs) != 1 {
		t.Fatalf("holds = %+v, want the share at S-OTHER", hs)
	}
	p, err := eng.PreviewCatidHold(hs[0].ID)
	testutil.MustNoErr(t, err, "preview")
	if p.Refusal == "" || len(p.Rows) != 1 || !p.Rows[0].WrongPart || p.Rows[0].CarrierPart != "PART-A" {
		t.Errorf("preview = %+v, want a refusal naming the PART-A cart", p)
	}
	if err := eng.ChargeCatidHold(hs[0].ID, "engineer"); err == nil {
		t.Error("charge onto another part's cart succeeded")
	}
	if got := runtimeOf(t, eng, n).RemainingUOPCached; got != 40 {
		t.Errorf("the cart = %d, want 40", got)
	}
	if len(openHolds(t, eng, pid)) != 1 {
		t.Error("a refused Charge closed the hold")
	}
}

// TestHold_ChargeRefusedOnAnUnmappedOrSharedCATID: the Charge is under the
// one style that runs the CATID; none or several is a refusal.
func TestHold_ChargeRefusedOnAnUnmappedOrSharedCATID(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := holdRig(t, "1003")
	strokeAt(eng, pid, a, 1)
	id := mustOneHold(t, eng.db, pid).ID
	if err := eng.ChargeCatidHold(id, "engineer"); err == nil {
		t.Error("charge of an unmapped CATID succeeded")
	}
	pinnedStyle(t, eng, pid, "STYLE-C", "1003")
	pinnedStyle(t, eng, pid, "STYLE-D", "1003")
	if err := eng.ChargeCatidHold(id, "engineer"); err == nil {
		t.Error("charge of a CATID two styles run succeeded")
	}
	if len(openHolds(t, eng, pid)) != 1 {
		t.Error("a refused Charge closed the hold")
	}
}

// TestHold_ReleaseNeverReemitsTheDelta: a release charges carts through the
// walk; no counter delta is emitted, so the hour, the snapshot rows and Core
// see the stroke once.
func TestHold_ReleaseNeverReemitsTheDelta(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := holdRig(t, "1003")
	strokeAt(eng, pid, a, 2)
	c := pinnedStyle(t, eng, pid, "STYLE-C", "1003")
	cartNode(t, eng, pid, c, "C-NODE", "PART-C", "PART-C", 9806)
	deltas := 0
	eng.Events.SubscribeTypes(func(Event) { deltas++ }, EventCounterDelta)
	testutil.MustNoErr(t, eng.ChargeCatidHold(mustOneHold(t, eng.db, pid).ID, "engineer"), "charge")
	if deltas != 0 {
		t.Errorf("the charge emitted %d counter deltas, want 0", deltas)
	}
	if n := hourlyUnder(t, eng, pid, c); n != 0 {
		t.Errorf("hourly under C = %d, want 0: the stroke was counted under A when it was made", n)
	}
}

// TestHold_SurvivesRestart: the ledger is a table; a restarted Edge reads the
// open hold and its strokes as they were.
func TestHold_SurvivesRestart(t *testing.T) {
	t.Parallel()
	path := copyEngineDBTemplate(t)
	db, err := store.OpenMigrated(path)
	testutil.MustNoErr(t, err, "open")
	pid, err := db.CreateProcess("HOLD-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	at := time.Now().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		_, err = db.HoldCatidDraw(pid, "1003", 0, "", holdReasonHeld, 2, at)
		testutil.MustNoErr(t, err, "hold")
	}
	testutil.MustNoErr(t, db.Close(), "close")

	db, err = store.OpenMigrated(path)
	testutil.MustNoErr(t, err, "reopen")
	t.Cleanup(func() { db.Close() })
	hs, err := db.ListOpenCatidHolds(pid)
	testutil.MustNoErr(t, err, "holds")
	if len(hs) != 1 || hs[0].Qty != 6 || hs[0].Strokes != 3 || hs[0].FirstTickAt.Unix() != at.Unix() {
		t.Errorf("after a restart: %+v, want one hold of 6 over 3 strokes, opened an hour ago", hs)
	}
}

// TestHold_AResolvedHoldTakesNoMoreStrokes: a stroke after a release opens a
// new hold; the released row is the record of what was released.
func TestHold_AResolvedHoldTakesNoMoreStrokes(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, err := db.CreateProcess("HOLD-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	opened, err := db.HoldCatidDraw(pid, "1003", 0, "", holdReasonHeld, 2, time.Now())
	testutil.MustNoErr(t, err, "hold")
	if !opened {
		t.Error("the first stroke did not report opening the hold")
	}
	first := mustOneHold(t, db, pid)
	qty, ok, err := db.ResolveCatidHold(first.ID, holdWrittenOff, "engineer", time.Now())
	testutil.MustNoErr(t, err, "resolve")
	if !ok || qty != 2 {
		t.Fatalf("resolve = %d, %t; want the 2 it latched", qty, ok)
	}
	_, ok, err = db.ResolveCatidHold(first.ID, holdCharged, "engineer", time.Now())
	testutil.MustNoErr(t, err, "second resolve")
	if ok {
		t.Error("a second resolve wrote")
	}
	opened, err = db.HoldCatidDraw(pid, "1003", 0, "", holdReasonHeld, 5, time.Now())
	testutil.MustNoErr(t, err, "stroke after")
	if !opened {
		t.Error("the stroke after the release did not open a new hold")
	}
	if h := mustOneHold(t, db, pid); h.ID == first.ID || h.Qty != 5 {
		t.Errorf("open hold = %+v, want a new one of 5", h)
	}
	released, err := db.GetCatidHold(first.ID)
	testutil.MustNoErr(t, err, "released row")
	if released.Qty != 2 || released.Resolution != holdWrittenOff {
		t.Errorf("released row = %+v, want 2, written off", released)
	}
}

// TestHold_LandingReleaseNeverDrawsAWrongPartCart: a held draw on B's part, B's
// positions holding a cart of B's part and a cart of another. "Mark as running"
// B lands it: the cart of B's part is charged, the other is not, and its share
// stays open for a person. Before, the landing release charged both carts.
func TestHold_LandingReleaseNeverDrawsAWrongPartCart(t *testing.T) {
	t.Parallel()
	eng, pid, _, b, _ := holdRig(t, "1001")
	same := cartNode(t, eng, pid, b, "B-SAME", "PART-B", "PART-B", 9741)
	other := cartNode(t, eng, pid, b, "B-OTHER", "PART-B", "PART-A", 9742)
	_, err := eng.db.HoldCatidDraw(pid, "1002", 0, "", holdReasonHeld, 1, time.Now().Add(-time.Minute))
	testutil.MustNoErr(t, err, "a held draw on 1002")
	testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &b), "mark B as running")
	if got := runtimeOf(t, eng, same).RemainingUOPCached; got != 39 {
		t.Errorf("B's cart of B's part = %d, want 39", got)
	}
	if got := runtimeOf(t, eng, other).RemainingUOPCached; got != 40 {
		t.Errorf("B's cart of another part = %d, want 40: a release never draws it", got)
	}
	hs := openHolds(t, eng, pid)
	if len(hs) != 1 || hs[0].ProcessNodeID != other || hs[0].Qty != 1 || hs[0].PayloadCode != "PART-B" {
		t.Errorf("open holds = %+v, want the other cart's share of 1 left for a person", hs)
	}
}

// TestHold_ChargeWalkNeverDrawsAWrongPartCart: the release's walk reads each
// cart itself: a cart of another part bound since the preview read it is not
// charged, and its share stays open. Before, the walk charged whatever cart
// was bound.
func TestHold_ChargeWalkNeverDrawsAWrongPartCart(t *testing.T) {
	t.Parallel()
	eng, pid, _, b, _ := holdRig(t, "1001")
	n := cartNode(t, eng, pid, b, "B-NODE", "PART-B", "PART-B", 9751)
	_, err := eng.db.HoldCatidDraw(pid, "1002", 0, "", holdReasonHeld, 2, time.Now())
	testutil.MustNoErr(t, err, "a held draw on 1002")
	h := mustOneHold(t, eng.db, pid)
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeLinesidePayload(n, "PART-A", true, "test"), "another part's cart")
	eng.chargeHold(&h, b)
	if got := runtimeOf(t, eng, n).RemainingUOPCached; got != 40 {
		t.Errorf("the other part's cart = %d, want 40", got)
	}
	shares := 0
	for _, o := range openHolds(t, eng, pid) {
		if o.ProcessNodeID == n && o.Qty == 2 && o.PayloadCode == "PART-B" {
			shares++
		}
	}
	if shares != 1 {
		t.Errorf("open holds = %+v, want the share of 2 at B-NODE left for a person", openHolds(t, eng, pid))
	}
}

// offPlanShareAt takes one off-plan stroke of 2 on 1003, S's part, with S's
// position X holding another part's cart: one share at X. Returns the share.
func offPlanShareAt(t *testing.T, eng *Engine, pid, a int64) (s, x int64, share store.CatidHold) {
	t.Helper()
	s = pinnedStyle(t, eng, pid, "STYLE-S", "1003")
	x = cartNode(t, eng, pid, s, "S-OTHER", "PART-S", "PART-A", 9761)
	if id := strokeAt(eng, pid, a, 2); id.Kind != plc.TickOffPlan {
		t.Fatalf("attribution = %+v, want off-plan", id)
	}
	share = mustOneHold(t, eng.db, pid)
	if share.ProcessNodeID != x {
		t.Fatalf("hold = %+v, want the share at S-OTHER", share)
	}
	return s, x, share
}

// TestHold_ShareTheChargeStyleDoesNotClaimStaysOpen: the CATID's share was
// taken at S's position; the CATID is then pinned to C, which does not claim
// it. The preview refuses naming the position, the Charge is refused and a
// landing on C leaves the share open. Before, both resolved it with nothing
// drawn.
func TestHold_ShareTheChargeStyleDoesNotClaimStaysOpen(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := holdRig(t, "1003")
	s, _, share := offPlanShareAt(t, eng, pid, a)
	testutil.MustNoErr(t, eng.db.SetStyleExpectedCATID(s, ""), "unpin S")
	c := pinnedStyle(t, eng, pid, "STYLE-C", "1003")
	cartNode(t, eng, pid, c, "C-NODE", "PART-C", "PART-C", 9762)
	p, err := eng.PreviewCatidHold(share.ID)
	testutil.MustNoErr(t, err, "preview")
	if p.Refusal == "" {
		t.Errorf("preview = %+v, want a refusal naming the position C does not claim", p)
	}
	if err := eng.ChargeCatidHold(share.ID, "engineer"); err == nil {
		t.Error("a Charge drawing nothing succeeded")
	}
	landOn(t, eng, pid, a, c)
	h, err := eng.db.GetCatidHold(share.ID)
	testutil.MustNoErr(t, err, "hold")
	if h.ResolvedAt != nil {
		t.Errorf("hold = %+v, want it open: nothing landed", h)
	}
}

// TestHold_ShareAtADeletedPositionIsRefused: a share whose position was
// deleted is refused in the preview and by the Charge; Write off is the way
// out. Before, the preview showed nothing and the Charge resolved it with
// nothing drawn.
func TestHold_ShareAtADeletedPositionIsRefused(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := holdRig(t, "1003")
	_, x, share := offPlanShareAt(t, eng, pid, a)
	testutil.MustNoErr(t, eng.db.DeleteProcessNode(x), "delete the position")
	p, err := eng.PreviewCatidHold(share.ID)
	testutil.MustNoErr(t, err, "preview")
	if p.Refusal == "" {
		t.Errorf("preview = %+v, want a refusal naming the deleted position", p)
	}
	if err := eng.ChargeCatidHold(share.ID, "engineer"); err == nil {
		t.Error("a Charge of a deleted position's share succeeded")
	}
	if len(openHolds(t, eng, pid)) != 1 {
		t.Error("a refused Charge closed the hold")
	}
}

// strokeOnConsume is the test's counter sink with one stroke's draw arriving
// the first time a walk charges a cart: a poll stroke landing between a
// release's read of a hold and its latch.
type strokeOnConsume struct {
	*fakeDeltaSink
	once   sync.Once
	stroke func()
}

func (s *strokeOnConsume) Consumed(ev uop.TickEvent) error {
	s.once.Do(s.stroke)
	return s.fakeDeltaSink.Consumed(ev)
}

// TestHold_ReleaseChargesWhatItLatched: two holds released on one landing; a
// stroke grows the second while the first is charged, after the release read
// it. The second is charged the quantity its latch resolved. Before, it was
// charged the quantity read, and the stroke's draw was resolved and never
// charged.
func TestHold_ReleaseChargesWhatItLatched(t *testing.T) {
	t.Parallel()
	eng, pid, _, _, _ := holdRig(t, "1003")
	c := pinnedStyle(t, eng, pid, "STYLE-C", "1003,1004")
	cNode := cartNode(t, eng, pid, c, "C-NODE", "PART-C", "PART-C", 9781)
	at := time.Now().Add(-time.Minute)
	for _, v := range []string{"1003", "1004"} {
		_, err := eng.db.HoldCatidDraw(pid, v, 0, "", holdReasonHeld, 2, at)
		testutil.MustNoErr(t, err, "hold "+v)
	}
	holds := openHolds(t, eng, pid)
	eng.SetInventoryDeltaSink(&strokeOnConsume{fakeDeltaSink: eng.inventoryDelta.(*fakeDeltaSink), stroke: func() {
		_, err := eng.db.HoldCatidDraw(pid, "1004", 0, "", holdReasonHeld, 2, time.Now())
		testutil.MustNoErr(t, err, "a stroke on 1004")
	}})
	testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &c), "mark C as running")
	var resolved int64
	for _, h := range holds {
		got, err := eng.db.GetCatidHold(h.ID)
		testutil.MustNoErr(t, err, "hold")
		if got.ResolvedAt == nil {
			t.Fatalf("hold %+v was not released", got)
		}
		resolved += got.Qty
	}
	if got := runtimeOf(t, eng, cNode).RemainingUOPCached; got != 40-int(resolved) {
		t.Errorf("C's cart = %d, want %d: every resolved unit charged", got, 40-int(resolved))
	}
}

// TestHold_OffPlanStrokeAtAnABPairHoldsOneShare: an off-plan stroke at an A/B
// pair whose carts are not its style's part holds one share, the lit side's:
// the dark side is skipped first, as for any stroke. Before, both sides held
// one.
func TestHold_OffPlanStrokeAtAnABPairHoldsOneShare(t *testing.T) {
	t.Parallel()
	r := newPairRig(t)
	lit := r.consume("O-A", "O-B", protocol.SwapModeSequential, true)
	r.consume("O-B", "O-A", protocol.SwapModeSequential, false)
	s, err := r.db.CreateStyle("OFF-STYLE", "", r.pid)
	testutil.MustNoErr(t, err, "style")
	for _, side := range [][2]string{{"O-A", "O-B"}, {"O-B", "O-A"}} {
		_, err := upsertClaimRetiredMode(r.db, processes.NodeClaimInput{StyleID: s, CoreNodeName: side[0], Role: "consume",
			SwapMode: protocol.SwapModeSequential, PayloadCode: "PART-S", UOPCapacity: 100, InboundSource: "SRC",
			PairedCoreNode: side[1]})
		testutil.MustNoErr(t, err, "claim "+side[0])
	}
	r.eng.handleCounterDelta(CounterDeltaEvent{ProcessID: r.pid, StyleID: s, Delta: 2, CATID: "1003", Kind: plc.TickOffPlan})
	if hs := openHolds(t, r.eng, r.pid); len(hs) != 1 || hs[0].ProcessNodeID != lit || hs[0].Qty != 2 {
		t.Errorf("holds = %+v, want one share of 2 at the lit side", hs)
	}
}

func mustOneHold(t *testing.T, db *store.DB, pid int64) store.CatidHold {
	t.Helper()
	hs, err := db.ListOpenCatidHolds(pid)
	testutil.MustNoErr(t, err, "holds")
	if len(hs) != 1 {
		t.Fatalf("open holds = %+v, want one", hs)
	}
	return hs[0]
}
