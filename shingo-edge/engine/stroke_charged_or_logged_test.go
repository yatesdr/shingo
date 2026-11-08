// stroke_charged_or_logged_test.go — decision 55: a stroke of part P charges
// the P cart standing at its position, into the negative if the count goes
// there; with no P cart there it is logged in the ledger against the position
// and the part, with the last cart released there, and nothing else ever
// happens to it. RED at the characterisation commit: every case.
package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/plc"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// noCartRows is the open "no cart" rows at a position.
func noCartRows(t *testing.T, eng *Engine, pid, nodeID int64) []store.CatidHold {
	t.Helper()
	var out []store.CatidHold
	for _, h := range openHolds(t, eng, pid) {
		if h.ProcessNodeID == nodeID && h.Reason == "no-cart" {
			out = append(out, h)
		}
	}
	return out
}

// TestPinNoCart_AStrokeAtAnEmptySeatIsLoggedTheNextCartLandsFull: two strokes
// at an empty seat are one "no cart" row of 2, the seat holds nothing, and the
// strip shows it as a count, not an alarm; the next cart lands at its full
// count and takes its own stroke, and the row stays.
func TestPinNoCart_AStrokeAtAnEmptySeatIsLoggedTheNextCartLandsFull(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	pid, nodeID, styleID, _ := emptySeat(t, eng, "NC-A", "PART-NA", 100)
	tickConsume(eng, pid, styleID, 1)
	tickConsume(eng, pid, styleID, 1)
	rows := noCartRows(t, eng, pid, nodeID)
	if len(rows) != 1 || rows[0].Qty != 2 || rows[0].Strokes != 2 || rows[0].PayloadCode != "PART-NA" {
		t.Fatalf("rows = %+v, want one no-cart row of 2 strokes of PART-NA", rows)
	}
	if rt := runtimeOf(t, eng, nodeID); rt.PendingUOPDelta != 0 {
		t.Errorf("the seat holds %d, want nothing", rt.PendingUOPDelta)
	}
	items, err := eng.CatidHealth(pid)
	testutil.MustNoErr(t, err, "the strip")
	var found *CatidHealthItem
	for i := range items {
		if items[i].Kind == catidKindHeld {
			found = &items[i]
		}
	}
	if found == nil || found.Severity != CatidHealthInfo ||
		found.Headline != "2 strokes of PART-NA at NC-A Node with no PART-NA cart — no cart released there yet" {
		t.Errorf("the strip's item = %+v, want the info count naming the position, the part and no cart off", found)
	}
	eng.HandleBinLanded(landing("NC-A-NODE", 8701, 1, 50, "PART-NA", 7))
	tickConsume(eng, pid, styleID, 1)
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 49 {
		t.Errorf("the next cart after one stroke = %d, want 49: it lands at its full count", got)
	}
	if rows := noCartRows(t, eng, pid, nodeID); len(rows) != 1 || rows[0].Qty != 2 {
		t.Errorf("rows after the landing = %+v, want the row of 2 still open", rows)
	}
}

// TestPinNoCart_AStrokeAtAKnownCartOfAnotherPartIsLoggedTheCartUnchanged: an
// ALT stroke at a cart known MIRROR-L is logged under MIRROR-R; the cart and
// Core's record of it are untouched.
func TestPinNoCart_AStrokeAtAKnownCartOfAnotherPartIsLoggedTheCartUnchanged(t *testing.T) {
	t.Parallel()
	eng, pid, _, alt, nodeID := otherPartRig(t, false, "MIRROR-L", true, nil)
	mark := deltaMark(eng)
	tickConsume(eng, pid, alt, 1)
	if rows := noCartRows(t, eng, pid, nodeID); len(rows) != 1 || rows[0].Qty != 1 || rows[0].PayloadCode != "MIRROR-R" {
		t.Errorf("rows = %+v, want one no-cart row of 1 MIRROR-R", rows)
	}
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 30 {
		t.Errorf("the MIRROR-L cart = %d, want 30", got)
	}
	if d := deltasSince(eng, mark); len(d) != 0 {
		t.Errorf("deltas %+v, want none", d)
	}
}

// TestPinNoCart_AnIncomingStrokeAtAnUnknownCartBoundBeforeTheStartIsLogged:
// under an open changeover, a cart that cannot say, bound before the start at
// a position whose part changes, is the departing part: the incoming stroke is
// logged, the cart untouched and nothing sent for Core to refuse.
func TestPinNoCart_AnIncomingStrokeAtAnUnknownCartBoundBeforeTheStartIsLogged(t *testing.T) {
	t.Parallel()
	eng, pid, _, alt, nodeID := unsaidUnderAChangeover(t, "before")
	mark := deltaMark(eng)
	tickConsume(eng, pid, alt, 1)
	if rows := noCartRows(t, eng, pid, nodeID); len(rows) != 1 || rows[0].Qty != 1 || rows[0].PayloadCode != "MIRROR-R" {
		t.Errorf("rows = %+v, want one no-cart row of 1 MIRROR-R", rows)
	}
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 30 {
		t.Errorf("the cart = %d, want 30", got)
	}
	if d := deltasSince(eng, mark); len(d) != 0 {
		t.Errorf("deltas %+v, want none", d)
	}
}

// TestPinNoCart_AStrokePastZeroAndTheReleaseRecordsIt: a stroke at the cart
// standing at 0 takes it to -1 and draws it; the release from the seat
// declares -1 there (no floor), marks it released empty, and the envelope and
// the disposition carry -1 (decision 55's record half: Core records the
// overpack; before it the wire was clamped to 0). The next stroke is logged,
// its row naming the cart released and its -1.
func TestPinNoCart_AStrokePastZeroAndTheReleaseRecordsIt(t *testing.T) {
	t.Parallel()
	eng, nodeID, acc, binID := fenceFixture(t, "NC-NEG")
	testutil.MustNoErr(t, eng.db.UpdateProcessNodeUOP(nodeID, 0), "the cart at 0")
	node, err := eng.db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "the node")
	claim := &processes.NodeClaim{Role: protocol.ClaimRoleConsume, PayloadCode: "PART-A"}
	tick := tickScope{processID: node.ProcessID, delta: 1, source: "test"}
	eng.handleConsumeTick(node, runtimeOf(t, eng, nodeID), claim, tick, unsaidCarrier{})
	if got := runtimeCount(t, eng, nodeID); got != -1 {
		t.Fatalf("the cart after a stroke at 0 = %d, want -1: no floor", got)
	}
	if got := chargedToBin(t, acc, binID); got != -1 {
		t.Errorf("charged to the bin %d, want -1", got)
	}
	wire := &protocol.UOPDisposition{Kind: protocol.DispositionReleasePartial}
	sent, _ := eng.declareReleaseCount(node, runtimeOf(t, eng, nodeID), binID, 0, ReleaseDisposition{Mode: DispositionSendPartialBack}, wire)
	rt := runtimeOf(t, eng, nodeID)
	if rt.RemainingUOPCached != -1 || !rt.ReleasedEmpty {
		t.Errorf("after the release: seat %d released_empty %t, want -1 and true (the release records what the cart reads)",
			rt.RemainingUOPCached, rt.ReleasedEmpty)
	}
	if sent == nil || *sent != -1 || wire.Count != -1 {
		t.Errorf("the envelope's count %v, the disposition's %d; want -1 both: the release sends what the cart reads", sent, wire.Count)
	}
	eng.handleConsumeTick(node, rt, claim, tick, unsaidCarrier{})
	rows := noCartRows(t, eng, node.ProcessID, nodeID)
	if len(rows) != 1 || rows[0].Qty != 1 || rows[0].LastBinID != binID || rows[0].LastDeclared != -1 || rows[0].LastLeftAt == nil {
		t.Errorf("rows = %+v, want one no-cart row of 1 naming bin %d released at -1", rows, binID)
	}
	if got := runtimeCount(t, eng, nodeID); got != -1 {
		t.Errorf("the released cart after a stroke = %d, want -1: it takes no stroke", got)
	}
}

// TestPinNoCart_ACartReleasedAtZeroThenStrokedIsLoggedTheCartStaysZero: a cart
// released empty takes no stroke; the strokes are logged, the row naming it as
// the last cart off at 0.
func TestPinNoCart_ACartReleasedAtZeroThenStrokedIsLoggedTheCartStaysZero(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	pid, nodeID, styleID, claimID := emptySeat(t, eng, "NC-Z", "PART-NZ", 100)
	releaseEmptyAt(t, eng, nodeID, claimID, "NC-Z-NODE", 8711, 4)
	tickConsume(eng, pid, styleID, 1)
	tickConsume(eng, pid, styleID, 1)
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 0 {
		t.Errorf("the cart released at 0 = %d after two strokes, want 0", got)
	}
	rows := noCartRows(t, eng, pid, nodeID)
	if len(rows) != 1 || rows[0].Qty != 2 || rows[0].LastBinID != 8711 || rows[0].LastDeclared != 0 {
		t.Errorf("rows = %+v, want one no-cart row of 2 naming bin 8711 released at 0", rows)
	}
	items, err := eng.CatidHealth(pid)
	testutil.MustNoErr(t, err, "the strip")
	saidIt := false
	for _, it := range items {
		if it.Kind == catidKindHeld && strings.HasPrefix(it.Headline, "2 strokes of PART-NZ at NC-Z Node with no PART-NZ cart — last cart off: bin 8711 at 0, ") {
			saidIt = true
		}
	}
	if !saidIt {
		t.Errorf("strip items %+v, want the count naming bin 8711 at 0 as the last cart off", items)
	}
}

// TestPinNoCart_TheRowsStayOpenAcrossACompletionAndALanding: a no-cart row
// and an off-plan share stay open through the changeover's completion onto
// the CATID's style and a landing of the part at the position: nothing lands
// a row by itself.
func TestPinNoCart_TheRowsStayOpenAcrossACompletionAndALanding(t *testing.T) {
	t.Parallel()
	eng, pid, a, b, _ := holdRig(t, "1002")
	node := mirrorNode(t, eng, pid, a, b, "NC-MIRROR", "MIRROR-L", "MIRROR-R", 9771)
	if id := strokeAt(eng, pid, a, 3); id.Kind != plc.TickOffPlan {
		t.Fatalf("attribution = %+v, want off-plan", id)
	}
	testutil.MustNoErr(t, eng.carrierLeft(node, "NC-MIRROR"), "the MIRROR-L cart lifted")
	coID, err := eng.changeoverService.Create(pid, &a, b, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "the changeover onto B")
	if id := strokeAt(eng, pid, a, 2); id.Kind != plc.TickTarget {
		t.Fatalf("attribution = %+v, want the target's", id)
	}
	before := openQty(t, eng, pid)
	if before != 5 {
		t.Fatalf("open before = %d, want 5 (3 off-plan, 2 with no cart)", before)
	}
	_, err = eng.completeCutover(pid, coID, triggeredManual, CutoverForced)
	testutil.MustNoErr(t, err, "the completion onto B")
	eng.HandleBinLanded(landing("NC-MIRROR", 9772, 1, 40, "MIRROR-R", 1<<40))
	if got := openQty(t, eng, pid); got != before {
		t.Errorf("open after the completion and the landing = %d, want %d: the rows stay", got, before)
	}
	if got := runtimeOf(t, eng, node).RemainingUOPCached; got != 40 {
		t.Errorf("the MIRROR-R cart = %d, want 40: it lands at its full count", got)
	}
}

// TestPinNoCart_ChargeAndWriteOffActOnALoggedRow: a person's Charge lands a
// no-cart row on the cart of its part now standing there, latched once; a
// Write off resolves another and charges nothing.
func TestPinNoCart_ChargeAndWriteOffActOnALoggedRow(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := holdRig(t, "1001")
	x := cartNode(t, eng, pid, a, "NC-X", "PART-A", "PART-A", 9781)
	y := cartNode(t, eng, pid, a, "NC-Y", "PART-A", "PART-A", 9782)
	for _, n := range []int64{x, y} {
		testutil.MustNoErr(t, eng.carrierLeft(n, map[int64]string{x: "NC-X", y: "NC-Y"}[n]), "the cart lifted")
	}
	if id := strokeAt(eng, pid, a, 2); id.Kind != plc.TickToday {
		t.Fatalf("attribution = %+v, want today", id)
	}
	rx, ry := noCartRows(t, eng, pid, x), noCartRows(t, eng, pid, y)
	if len(rx) != 1 || len(ry) != 1 || rx[0].CATID != "1001" {
		t.Fatalf("rows at NC-X %+v and NC-Y %+v, want one no-cart row each on CATID 1001", rx, ry)
	}
	eng.HandleBinLanded(landing("NC-X", 9783, 1, 40, "PART-A", 1<<40))
	testutil.MustNoErr(t, eng.ChargeCatidHold(rx[0].ID, "engineer"), "Charge")
	if got := runtimeOf(t, eng, x).RemainingUOPCached; got != 38 {
		t.Errorf("NC-X's cart after the Charge = %d, want 38", got)
	}
	if err := eng.ChargeCatidHold(rx[0].ID, "engineer"); err != ErrHoldResolved {
		t.Errorf("a second Charge = %v, want ErrHoldResolved", err)
	}
	testutil.MustNoErr(t, eng.WriteOffCatidHold(ry[0].ID, "engineer"), "Write off")
	h, err := eng.db.GetCatidHold(ry[0].ID)
	testutil.MustNoErr(t, err, "the written-off row")
	if h == nil || h.Resolution != holdWrittenOff || h.Qty != 2 {
		t.Errorf("the written-off row = %+v, want written off, 2", h)
	}
}
