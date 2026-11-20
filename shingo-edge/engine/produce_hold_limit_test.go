package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// produce_hold_limit_test.go — a strict claim's hold limit (hold_limit): the
// most its seat sets aside off the cart, what the line's packer, conveyor or
// accumulator holds while the cart is full or swapping.
//
// SPR ALN_011 2026-11-19: the limit was one pack (132). Each cart's wait made a
// pile of 117 to 119, under it, which the next cart took on its first stroke,
// so the count read 132 minutes after a swap and auto-reorder asked for the
// next one at once.

const holdLimit = 20

// limitSeat is strictSeatOn (pack 132, bin 7301 at count, no hold) with the
// claim's hold_limit set to limit.
func limitSeat(t *testing.T, count, limit int) (eng *Engine, sink *fakeDeltaSink, processID, nodeID, styleID int64) {
	t.Helper()
	db := testEngineDB(t)
	eng, sink, processID, nodeID, styleID, claimID := strictSeatOn(t, db, strictPack, count, 0, "", true)
	_, err := db.DB.Exec(`UPDATE style_node_claims SET hold_limit=? WHERE id=?`, limit, claimID)
	testutil.MustNoErr(t, err, "set hold_limit")
	return eng, sink, processID, nodeID, styleID
}

// TestHoldLimit_PastItThePileIsThrownAwayAndTheNextCartStartsClean: a full
// cart (132 of 132) on a claim limited to 20. Twenty strokes are set aside. The
// 21st throws the whole pile of 20 away with one hold_cap row and stops the
// seat, so the strokes after it are set aside nowhere. The next cart binds and
// counts only its own strokes.
func TestHoldLimit_PastItThePileIsThrownAwayAndTheNextCartStartsClean(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID := limitSeat(t, strictPack, holdLimit)
	for range holdLimit {
		charSPTick(eng, processID, styleID, 1)
	}
	assertSeat(t, eng, nodeID, strictPack, holdLimit, "WIDGET-A")
	assertDrops(t, eng.db)

	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, strictPack, 0, "")
	if !runtimeOf(t, eng, nodeID).HoldStopped {
		t.Error("the seat is not stopped after the pile passed the hold limit")
	}
	assertDrops(t, eng.db, protocol.HoldDropped{Reason: protocol.HoldDropHoldCap, Qty: holdLimit, PayloadCode: "WIDGET-A",
		Node: "PRODUCE-NODE", CalledBy: "edge:tick"})
	for range 97 {
		charSPTick(eng, processID, styleID, 1)
	}
	assertSeat(t, eng, nodeID, strictPack, 0, "")

	b2 := int64(7302)
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeWithBin(nodeID, nil, &b2, 0), "the next cart binds empty")
	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 1, 0, "")
	if len(sink.binCalls) != 1 || emitted(t, sink) != 1 {
		t.Errorf("deltas = %+v, want one produce_tick of 1: the next cart takes its own stroke and nothing else", sink.binCalls)
	}
}

// TestHoldLimit_APileWithinItStillLandsOnTheNextCart: the parts a packer holds
// through a swap still belong to the next cart. Fifteen strokes beside a full
// cart, within the limit of 20, are kept. The next cart's first stroke books
// all 16.
func TestHoldLimit_APileWithinItStillLandsOnTheNextCart(t *testing.T) {
	t.Parallel()
	eng, sink, processID, nodeID, styleID := limitSeat(t, strictPack, holdLimit)
	for range 15 {
		charSPTick(eng, processID, styleID, 1)
	}
	b2 := int64(7302)
	testutil.MustNoErr(t, eng.db.SetProcessNodeActiveBinID(nodeID, &b2), "the next cart binds")
	testutil.MustNoErr(t, eng.db.UpdateProcessNodeUOP(nodeID, 0), "it lands empty")
	charSPTick(eng, processID, styleID, 1)
	assertSeat(t, eng, nodeID, 16, 0, "")
	assertDrops(t, eng.db)
	if emitted(t, sink) != 16 {
		t.Errorf("deltas = %+v, want 16 booked on the next cart", sink.binCalls)
	}
}

// TestHoldLimit_HoldCap is the decider's table: the limit counts only on a
// strict claim with a pack and only below the pack; otherwise one pack.
func TestHoldLimit_HoldCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		strict bool
		pack   int
		limit  int
		want   int
	}{
		{"no limit is one pack", true, strictPack, 0, strictPack},
		{"a limit below the pack", true, strictPack, holdLimit, holdLimit},
		{"a limit at the pack", true, strictPack, strictPack, strictPack},
		{"a limit past the pack is one pack", true, strictPack, 500, strictPack},
		{"strict pack off: no reader", false, strictPack, holdLimit, strictPack},
		{"no pack", true, 0, holdLimit, 0},
	} {
		claim := &processes.NodeClaim{Role: protocol.ClaimRoleProduce, PayloadCode: "WIDGET-A",
			StrictPack: tc.strict, UOPCapacity: tc.pack, HoldLimit: tc.limit}
		if got := holdCap(claim); got != tc.want {
			t.Errorf("%s: holdCap = %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := holdCap(nil); got != 0 {
		t.Errorf("holdCap(nil) = %d, want 0", got)
	}
}
