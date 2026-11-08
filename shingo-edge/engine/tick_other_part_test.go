// tick_other_part_test.go — a consume stroke never draws a cart of another
// part: a bound carrier recorded as a part the tick's claim does not take takes
// no tick; the stroke is logged at the position (decision 55).
package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

const otherPartNode = "OP-P4"

// otherPartRig is a process with RUN (MIRROR-L at OP-P4) and ALT (MIRROR-R
// there, or the parts altAllowed lists), its seat holding cart 9901 at 30
// recorded as part (known), or unknown when known is false. The process runs
// ALT, or RUN when onRun.
func otherPartRig(t *testing.T, onRun bool, part string, known bool, altAllowed []string) (eng *Engine, pid, run, alt, nodeID int64) {
	t.Helper()
	db := testEngineDB(t)
	eng = testEngine(t, db)
	eng.wireEventHandlers()
	pid, err := db.CreateProcess("OP-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	run, err = db.CreateStyle("RUN", "", pid)
	testutil.MustNoErr(t, err, "RUN")
	alt, err = db.CreateStyle("ALT", "", pid)
	testutil.MustNoErr(t, err, "ALT")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(run, "1001"), "pin RUN")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(alt, "1002"), "pin ALT")
	nodeID, err = db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: otherPartNode, Code: "OP4", Name: otherPartNode, Enabled: true})
	testutil.MustNoErr(t, err, "node")
	runClaim, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{StyleID: run, CoreNodeName: otherPartNode,
		Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSimple, PayloadCode: "MIRROR-L", UOPCapacity: 40, InboundSource: "SRC"})
	testutil.MustNoErr(t, err, "RUN claim")
	altClaim, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{StyleID: alt, CoreNodeName: otherPartNode,
		Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSimple, PayloadCode: "MIRROR-R", UOPCapacity: 40, InboundSource: "SRC",
		AllowedPayloadCodes: altAllowed})
	testutil.MustNoErr(t, err, "ALT claim")
	active, claim := alt, altClaim
	if onRun {
		active, claim = run, runClaim
	}
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &active), "active style")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claim, binPtr(9901), 30), "the cart")
	if known {
		testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(nodeID, part, true, "landing"), "the cart's part")
	}
	return eng, pid, run, alt, nodeID
}

// wantSeat fails unless the seat's cart reads count, nothing is held, and the
// ledger has logged at the position.
func wantSeat(t *testing.T, eng *Engine, pid, nodeID int64, what string, count int, logged int64) {
	t.Helper()
	wantSeat55(t, eng, pid, nodeID, what, seat55{cart: count, logged: logged})
}

// deltasSince is the bin deltas the sink recorded after mark.
func deltasSince(eng *Engine, mark int) []fakeBinCall {
	sink := eng.inventoryDelta.(*fakeDeltaSink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]fakeBinCall(nil), sink.binCalls[mark:]...)
}

func deltaMark(eng *Engine) int {
	sink := eng.inventoryDelta.(*fakeDeltaSink)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return len(sink.binCalls)
}

// TestPinFlip_AStrokeNeverDrawsACartOfAnotherPart: an ALT stroke at a cart
// recorded MIRROR-L (by its landing, or by the flip that left it) is logged at
// the position and the cart and Core's record of it are untouched; a cart that
// cannot say is charged as the claim's part, as before; a cart of an allowed
// alternate is the part. A MIRROR-R cart that lands, or the standing cart once
// a person names it MIRROR-R, takes the next stroke from its own count,
// stamped MIRROR-R; the logged strokes stay in the ledger (decision 55).
// Before decision 38, every ALT stroke was charged to the MIRROR-L cart,
// stamped MIRROR-L, and Core accepted it: the wrong cart.
func TestPinFlip_AStrokeNeverDrawsACartOfAnotherPart(t *testing.T) {
	t.Parallel()
	t.Run("a cart known MIRROR-L from its landing: logged", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := otherPartRig(t, false, "MIRROR-L", true, nil)
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, pid, nodeID, "after an ALT stroke", 30, 1)
		if d := deltasSince(eng, mark); len(d) != 0 {
			t.Errorf("deltas %+v, want none: the MIRROR-L cart is not drawn", d)
		}
	})
	t.Run("a cart recorded MIRROR-L by the flip: logged", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := otherPartRig(t, true, "", false, nil)
		testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &alt), "the switch to ALT")
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, pid, nodeID, "after an ALT stroke", 30, 1)
		if d := deltasSince(eng, mark); len(d) != 0 {
			t.Errorf("deltas %+v, want none: the cart the flip left is MIRROR-L", d)
		}
	})
	t.Run("a cart that cannot say: charged as the claim's part", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := otherPartRig(t, false, "", false, nil)
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, pid, nodeID, "after an ALT stroke", 29, 0)
		if d := deltasSince(eng, mark); len(d) != 1 || d[0].BinID != 9901 || d[0].Delta != -1 || d[0].PayloadCode != "MIRROR-R" {
			t.Errorf("deltas %+v, want -1 on 9901 stamped MIRROR-R", d)
		}
	})
	t.Run("a cart of an allowed alternate: charged", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := otherPartRig(t, false, "MIRROR-R2", true, []string{"MIRROR-R", "MIRROR-R2"})
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, pid, nodeID, "after an ALT stroke", 29, 0)
		if d := deltasSince(eng, mark); len(d) != 1 || d[0].BinID != 9901 || d[0].Delta != -1 || d[0].PayloadCode != "MIRROR-R2" {
			t.Errorf("deltas %+v, want -1 on 9901 stamped MIRROR-R2", d)
		}
	})
	t.Run("a MIRROR-R landing: the new cart takes its own strokes", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := otherPartRig(t, false, "MIRROR-L", true, nil)
		tickConsume(eng, pid, alt, 1)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, pid, nodeID, "after two ALT strokes", 30, 2)
		testutil.MustNoErr(t, eng.carrierLeft(nodeID, otherPartNode), "the MIRROR-L cart leaves")
		eng.HandleBinLanded(landing(otherPartNode, 9911, 1, 40, "MIRROR-R", 7))
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, pid, nodeID, "the MIRROR-R cart after one stroke", 39, 2)
		if d := deltasSince(eng, mark); len(d) != 1 || d[0].BinID != 9911 || d[0].Delta != -1 || d[0].PayloadCode != "MIRROR-R" {
			t.Errorf("deltas %+v, want -1 on 9911 stamped MIRROR-R", d)
		}
	})
	t.Run("a person names the standing cart MIRROR-R: the next stroke draws it", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := otherPartRig(t, false, "MIRROR-L", true, nil)
		tickConsume(eng, pid, alt, 1)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, pid, nodeID, "after two ALT strokes", 30, 2)
		eng.HandleUOPAdjustment(protocol.UOPAdjustment{CoreNodeName: otherPartNode, BinID: 9901, NewRemaining: 30,
			Actor: "admin", PayloadCode: "MIRROR-R", PayloadKnown: true})
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, pid, nodeID, "the named cart after one stroke", 29, 2)
		if d := deltasSince(eng, mark); len(d) != 1 || d[0].BinID != 9901 || d[0].Delta != -1 || d[0].PayloadCode != "MIRROR-R" {
			t.Errorf("deltas %+v, want -1 on 9901 stamped MIRROR-R", d)
		}
	})
}

// unsaidUnderAChangeover is otherPartRig on RUN with a cart that cannot say
// its part, and a changeover to ALT opened with nothing to do. bound is when
// the cart was bound: "before" the start (the landing that could not name it,
// then the changeover), "since" (the changeover, dated two seconds back, then
// the landing), or "" with no changeover opened.
func unsaidUnderAChangeover(t *testing.T, bound string) (eng *Engine, pid, run, alt, nodeID int64) {
	t.Helper()
	eng, pid, run, alt, nodeID = otherPartRig(t, true, "", false, nil)
	unnamed := func() {
		eng.recordLinesideCarrier(nodeID, otherPartNode, domain.UnknownCarrier(), domain.CarrierFromLanding)
	}
	switch bound {
	case "before":
		unnamed()
		_, err := eng.changeoverService.Create(pid, &run, alt, "test", "", nil, nil, nil, nil)
		testutil.MustNoErr(t, err, "the changeover to ALT")
	case "since":
		coID, err := eng.changeoverService.Create(pid, &run, alt, "test", "", nil, nil, nil, nil)
		testutil.MustNoErr(t, err, "the changeover to ALT")
		openedBefore(t, eng, coID)
		unnamed()
	default:
		unnamed()
	}
	return eng, pid, run, alt, nodeID
}

// TestPinChangeover_ACartThatCannotSayLogsTheIncomingStrokes: with a
// changeover open to ALT, the cart standing at a position whose part changes
// (MIRROR-L to MIRROR-R) that cannot say its part, bound before the
// changeover started, is the departing part (decision 52's reading): an ALT
// stroke is logged at the position (decision 55), the cart's count unchanged
// and nothing sent for Core to refuse; a MIRROR-R cart that lands takes its
// own strokes from its full count; RUN's stroke draws the cart as before. A cart bound since the start came with the changeover and is
// charged as ALT's part (as the flip reads it, TestFlip_RecordsTheCarrierItLeaves);
// with no changeover open the cart is charged as the claim's part, as before.
// Before, an ALT stroke under the open changeover was charged to the cart that
// cannot say, stamped MIRROR-R, and Core, which knows the cart as MIRROR-L,
// refused the delta.
func TestPinChangeover_ACartThatCannotSayLogsTheIncomingStrokes(t *testing.T) {
	t.Parallel()
	t.Run("bound before the start: an ALT stroke is logged", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := unsaidUnderAChangeover(t, "before")
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, pid, nodeID, "after an ALT stroke", 30, 1)
		if d := deltasSince(eng, mark); len(d) != 0 {
			t.Errorf("deltas %+v, want none: the cart is MIRROR-L, and Core would refuse a MIRROR-R draw on it", d)
		}
		testutil.MustNoErr(t, eng.carrierLeft(nodeID, otherPartNode), "the cart leaves")
		eng.HandleBinLanded(landing(otherPartNode, 9911, 1, 40, "MIRROR-R", 7))
		mark = deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, pid, nodeID, "the MIRROR-R cart after one stroke", 39, 1)
		if d := deltasSince(eng, mark); len(d) != 1 || d[0].BinID != 9911 || d[0].Delta != -1 || d[0].PayloadCode != "MIRROR-R" {
			t.Errorf("deltas %+v, want -1 on 9911 stamped MIRROR-R", d)
		}
	})
	t.Run("bound before the start: RUN's stroke draws the cart", func(t *testing.T) {
		t.Parallel()
		eng, pid, run, _, nodeID := unsaidUnderAChangeover(t, "before")
		mark := deltaMark(eng)
		tickConsume(eng, pid, run, 1)
		wantSeat(t, eng, pid, nodeID, "after a RUN stroke", 29, 0)
		if d := deltasSince(eng, mark); len(d) != 1 || d[0].BinID != 9901 || d[0].Delta != -1 || d[0].PayloadCode != "MIRROR-L" {
			t.Errorf("deltas %+v, want -1 on 9901 stamped MIRROR-L", d)
		}
	})
	t.Run("bound since the start: an ALT stroke is charged as ALT's part", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := unsaidUnderAChangeover(t, "since")
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, pid, nodeID, "after an ALT stroke", 29, 0)
		if d := deltasSince(eng, mark); len(d) != 1 || d[0].BinID != 9901 || d[0].Delta != -1 || d[0].PayloadCode != "MIRROR-R" {
			t.Errorf("deltas %+v, want -1 on 9901 stamped MIRROR-R", d)
		}
	})
	t.Run("no changeover open: an ALT stroke is charged as the claim's part", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := unsaidUnderAChangeover(t, "")
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, pid, nodeID, "after an ALT stroke", 29, 0)
		if d := deltasSince(eng, mark); len(d) != 1 || d[0].BinID != 9901 || d[0].Delta != -1 || d[0].PayloadCode != "MIRROR-R" {
			t.Errorf("deltas %+v, want -1 on 9901 stamped MIRROR-R", d)
		}
	})
}

// TestPreview_TheRefusalSaysWhatTheCartIsRecordedAs: a Charge of an ALT share
// at a cart of another part is refused with what the cart is recorded as and
// by what, so a person knows to name the cart or land one of the part; the row
// carries who recorded it. Before, it said only that the cart was not ALT's.
func TestPreview_TheRefusalSaysWhatTheCartIsRecordedAs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		source, said string
	}{
		{"flip", "from the switch off RUN"},
		{"landing", "from the delivery"},
		{"operator", "by the operator"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			t.Parallel()
			eng, pid, _, alt, nodeID := otherPartRig(t, true, "", false, nil)
			testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &alt), "the switch to ALT")
			if tc.source != "flip" {
				testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeLinesidePayload(nodeID, "MIRROR-L", true, tc.source), "the cart's record")
			}
			_, err := eng.db.HoldCatidDraw(pid, "1002", nodeID, "MIRROR-R", holdReasonOffPlan, 4, time.Now())
			testutil.MustNoErr(t, err, "an ALT share held")
			pv, err := eng.PreviewCatidHold(openHolds(t, eng, pid)[0].ID)
			testutil.MustNoErr(t, err, "preview")
			if want := "the cart at OP-P4 is recorded as MIRROR-L (" + tc.said + "), not ALT's part"; pv.Refusal != want {
				t.Errorf("refusal = %q, want %q", pv.Refusal, want)
			}
			if len(pv.Rows) != 1 || pv.Rows[0].CarrierPart != "MIRROR-L" || pv.Rows[0].CarrierSource != tc.source || !pv.Rows[0].WrongPart {
				t.Errorf("rows = %+v, want the MIRROR-L cart recorded by %s, the wrong part", pv.Rows, tc.source)
			}
		})
	}
}
