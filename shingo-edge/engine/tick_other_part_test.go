// tick_other_part_test.go — a consume stroke never draws a cart of another
// part: a bound carrier recorded as a part the tick's claim does not take takes
// no tick; the stroke is held at the seat for a cart of its part, and the
// stranded notice names the seat.
package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
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

// wantSeat fails unless the seat's cart reads count with hold of holdPart.
func wantSeat(t *testing.T, eng *Engine, nodeID int64, what string, count int, hold int64, holdPart string) {
	t.Helper()
	rt := runtimeOf(t, eng, nodeID)
	if rt.RemainingUOPCached != count || rt.PendingUOPDelta != hold || (hold != 0 && rt.PendingUOPPayload != holdPart) {
		t.Errorf("%s: cart %d, hold %d of %q; want %d, hold %d of %q", what, rt.RemainingUOPCached, rt.PendingUOPDelta,
			rt.PendingUOPPayload, count, hold, holdPart)
	}
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
// recorded MIRROR-L (by its landing, or by the flip that left it) is held at the
// seat under MIRROR-R and the cart and Core's record of it are untouched; a
// cart that cannot say is charged as the claim's part, as before; a cart of an
// allowed alternate is the part. The hold replays onto a MIRROR-R cart that
// lands, or onto the standing cart once a person names it MIRROR-R, at the next
// stroke, stamped MIRROR-R. Before, every ALT stroke was charged to the
// MIRROR-L cart, stamped MIRROR-L, and Core accepted it: the wrong cart.
func TestPinFlip_AStrokeNeverDrawsACartOfAnotherPart(t *testing.T) {
	t.Parallel()
	t.Run("a cart known MIRROR-L from its landing: held under MIRROR-R", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := otherPartRig(t, false, "MIRROR-L", true, nil)
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, nodeID, "after an ALT stroke", 30, 1, "MIRROR-R")
		if d := deltasSince(eng, mark); len(d) != 0 {
			t.Errorf("deltas %+v, want none: the MIRROR-L cart is not drawn", d)
		}
	})
	t.Run("a cart recorded MIRROR-L by the flip: held under MIRROR-R", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := otherPartRig(t, true, "", false, nil)
		testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &alt), "the switch to ALT")
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, nodeID, "after an ALT stroke", 30, 1, "MIRROR-R")
		if d := deltasSince(eng, mark); len(d) != 0 {
			t.Errorf("deltas %+v, want none: the cart the flip left is MIRROR-L", d)
		}
	})
	t.Run("a cart that cannot say: charged as the claim's part", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := otherPartRig(t, false, "", false, nil)
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, nodeID, "after an ALT stroke", 29, 0, "")
		if d := deltasSince(eng, mark); len(d) != 1 || d[0].BinID != 9901 || d[0].Delta != -1 || d[0].PayloadCode != "MIRROR-R" {
			t.Errorf("deltas %+v, want -1 on 9901 stamped MIRROR-R", d)
		}
	})
	t.Run("a cart of an allowed alternate: charged", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := otherPartRig(t, false, "MIRROR-R2", true, []string{"MIRROR-R", "MIRROR-R2"})
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, nodeID, "after an ALT stroke", 29, 0, "")
		if d := deltasSince(eng, mark); len(d) != 1 || d[0].BinID != 9901 || d[0].Delta != -1 || d[0].PayloadCode != "MIRROR-R2" {
			t.Errorf("deltas %+v, want -1 on 9901 stamped MIRROR-R2", d)
		}
	})
	t.Run("a MIRROR-R landing: the hold replays onto the new cart", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := otherPartRig(t, false, "MIRROR-L", true, nil)
		tickConsume(eng, pid, alt, 1)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, nodeID, "after two ALT strokes", 30, 2, "MIRROR-R")
		testutil.MustNoErr(t, eng.carrierLeft(nodeID, otherPartNode), "the MIRROR-L cart leaves")
		eng.HandleBinLanded(landing(otherPartNode, 9911, 1, 40, "MIRROR-R", 7))
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, nodeID, "the MIRROR-R cart after one stroke", 37, 0, "")
		if d := deltasSince(eng, mark); len(d) != 1 || d[0].BinID != 9911 || d[0].Delta != -3 || d[0].PayloadCode != "MIRROR-R" {
			t.Errorf("deltas %+v, want -3 on 9911 stamped MIRROR-R (the 2 held and the 1)", d)
		}
	})
	t.Run("a person names the standing cart MIRROR-R: the next stroke replays the hold onto it", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := otherPartRig(t, false, "MIRROR-L", true, nil)
		tickConsume(eng, pid, alt, 1)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, nodeID, "after two ALT strokes", 30, 2, "MIRROR-R")
		eng.HandleUOPAdjustment(protocol.UOPAdjustment{CoreNodeName: otherPartNode, BinID: 9901, NewRemaining: 30,
			Actor: "admin", PayloadCode: "MIRROR-R", PayloadKnown: true})
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		wantSeat(t, eng, nodeID, "the named cart after one stroke", 27, 0, "")
		if d := deltasSince(eng, mark); len(d) != 1 || d[0].BinID != 9901 || d[0].Delta != -3 || d[0].PayloadCode != "MIRROR-R" {
			t.Errorf("deltas %+v, want -3 on 9901 stamped MIRROR-R (the 2 held and the 1)", d)
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

// TestStrandedMonitor_NamesAHoldBesideACartOfAnotherPart: a hold rising beside
// a cart the flip recorded as MIRROR-L, under ALT's claim of MIRROR-R, is
// watched as an unbound seat's is, and the notice says where, what the cart is
// recorded as and by what, what the style draws, and how many are held. Before,
// a bound seat was never watched.
func TestStrandedMonitor_NamesAHoldBesideACartOfAnotherPart(t *testing.T) {
	t.Parallel()
	eng, pid, _, alt, _ := otherPartRig(t, true, "", false, nil)
	testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &alt), "the switch to ALT")
	var fired []UOPStrandedEvent
	eng.Events.SubscribeTypes(func(evt Event) {
		if a, ok := evt.Payload.(UOPStrandedEvent); ok {
			fired = append(fired, a)
		}
	}, EventUOPStranded)
	sm := &strandedMonitor{eng: eng, states: map[int64]*strandedNodeState{}}
	base := time.Now()
	for i := 0; i < strandedWindow+1; i++ {
		tickConsume(eng, pid, alt, 3)
		sm.tick(base.Add(time.Duration(i) * time.Minute))
	}
	const want = "Strokes held at OP-P4 — the cart there is recorded as MIRROR-L (from the switch off RUN); ALT draws " +
		"MIRROR-R here. 12 held for the next MIRROR-R cart: land one, or name this cart with Load Payload."
	if got := eng.StrandedAlarmDetail(otherPartNode); got != want {
		t.Errorf("notice = %q\nwant      %q", got, want)
	}
	if len(fired) != 1 || fired[0].PendingDelta != 12 || fired[0].Detail != want {
		t.Errorf("events = %+v, want one with the notice and 12 held", fired)
	}
	testutil.MustNoErr(t, eng.carrierLeft(mustNodeIDByName(t, eng, otherPartNode), otherPartNode), "the MIRROR-L cart leaves")
	eng.HandleBinLanded(landing(otherPartNode, 9912, 1, 40, "MIRROR-R", 9))
	tickConsume(eng, pid, alt, 1)
	sm.tick(base.Add(time.Duration(strandedWindow+1) * time.Minute))
	if got := eng.StrandedAlarmDetail(otherPartNode); got != "" {
		t.Errorf("after the MIRROR-R cart landed and took the hold, notice = %q, want none", got)
	}
}
