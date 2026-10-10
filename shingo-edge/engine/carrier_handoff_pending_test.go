package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// carrier_handoff_pending_test.go — Lane H's Edge pins that need the landing
// itself (H.1: protocol.BinLanded on protocol.SubjectBinLanded,
// "transit.bin_landed"; D10: its placement stamp). None of that existed at the
// base. It was behind the laneh_pending tag until the source landed; the
// names it assumed — protocol.BinLanded{BinID, CoreNodeName, Count, Epoch,
// PayloadCode, PayloadKnown, OrderUUID, PlacedAt, PlacedSeq} and
// Engine.HandleBinLanded — are the names the source uses.

func landing(node string, binID, epoch int64, count int, part string, seq int64) protocol.BinLanded {
	return protocol.BinLanded{
		BinID: binID, CoreNodeName: node, Count: count, Epoch: epoch,
		PayloadCode: part, PayloadKnown: part != "", OrderUUID: "uuid-landing",
		PlacedAt: time.Now().UTC(), PlacedSeq: seq,
	}
}

// TestPinH5_LandingSubjectThenDeliveredKeepsTheTicks — H.1/H.5: the landing
// binds; the delivered binds nothing and writes no count.
func TestPinH5_LandingSubjectThenDeliveredKeepsTheTicks(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	processID, nodeID, styleID, _ := emptySeat(t, eng, "HPL1", "PART-HPL1", 100)

	const bin, epoch, n, k = int64(9101), int64(4), 100, 3
	eng.HandleBinLanded(landing("HPL1-NODE", bin, epoch, n, "PART-HPL1", 11))
	for i := 0; i < k; i++ {
		tickConsume(eng, processID, styleID, 1)
	}
	deliver(t, eng, nodeID, "uuid-hpl1", "HPL1-NODE", bin, epoch, n)

	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != n-k {
		t.Errorf("count = %d, want %d (H.5: delivered writes no count)", got, n-k)
	}
}

// TestPinH4_RedeliveredLandingSubjectKeepsTheTicks — H.4 / D10: the same
// landing again (same placement stamp) is refused by the bind's WHERE.
func TestPinH4_RedeliveredLandingSubjectKeepsTheTicks(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	processID, nodeID, styleID, _ := emptySeat(t, eng, "HPL2", "PART-HPL2", 100)

	l := landing("HPL2-NODE", 9102, 4, 100, "PART-HPL2", 12)
	eng.HandleBinLanded(l)
	for i := 0; i < 3; i++ {
		tickConsume(eng, processID, styleID, 1)
	}
	eng.HandleBinLanded(l)

	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 97 {
		t.Errorf("count = %d, want 97: the redelivered landing rewrote the count", got)
	}
}

// TestPinD10_AnOlderLandingDoesNotDisplaceANewerCarrier — D10 (DESIGN-H §5 L8):
// a late landing for a carrier that has since been replaced carries an older
// placement stamp than the seat's and is refused.
func TestPinD10_AnOlderLandingDoesNotDisplaceANewerCarrier(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, _ := emptySeat(t, eng, "HPL3", "PART-HPL3", 100)

	eng.HandleBinLanded(landing("HPL3-NODE", 9104, 1, 70, "PART-HPL3", 21))
	eng.HandleBinLanded(landing("HPL3-NODE", 9103, 1, 100, "PART-HPL3", 20)) // the older drop, late

	rt := runtimeOf(t, eng, nodeID)
	if rt.ActiveBinID == nil || *rt.ActiveBinID != 9104 || rt.RemainingUOPCached != 70 {
		t.Errorf("seat = bin %v count %d, want 9104/70: an older placement displaced the newer carrier",
			rt.ActiveBinID, rt.RemainingUOPCached)
	}
}

// TestPinH1_TheLandingRecordsThePartAsKnown — H.1 (part with a known flag) and
// H.5 (recordDeliveredCarrier re-homed onto the landing): the landing's part
// is the seat's identity, known. The base's Bound arm records UNKNOWN
// (TestCarrierBound_ByAdjustmentIsUnknown), which is the door this replaces.
func TestPinH1_TheLandingRecordsThePartAsKnown(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, _ := emptySeat(t, eng, "HPL4", "PART-HPL4", 100)

	eng.HandleBinLanded(landing("HPL4-NODE", 9105, 2, 100, "PART-HPL4", 31))

	rt := runtimeOf(t, eng, nodeID)
	if rt.LinesidePayloadCode != "PART-HPL4" || !rt.LinesidePayloadKnown {
		t.Errorf("identity = %q known=%v, want PART-HPL4 known", rt.LinesidePayloadCode, rt.LinesidePayloadKnown)
	}

	// A landing that says the carrier's part is not known leaves it UNKNOWN,
	// never the previous occupant's.
	testutil.MustNoErr(t, eng.carrierLeft(nodeID, "HPL4-NODE"), "the lift")
	eng.HandleBinLanded(landing("HPL4-NODE", 9106, 1, 0, "", 32))
	rt = runtimeOf(t, eng, nodeID)
	if rt.LinesidePayloadCode != "" || rt.LinesidePayloadKnown {
		t.Errorf("identity = %q known=%v, want unknown", rt.LinesidePayloadCode, rt.LinesidePayloadKnown)
	}
}

// TestControlH_EmptySeatLandingSubjectSeedsCoresCount — CONTROL with the
// landing subject: an empty seat binds with Core's count and generation.
func TestControlH_EmptySeatLandingSubjectSeedsCoresCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	_, nodeID, _, _ := emptySeat(t, eng, "HPL5", "PART-HPL5", 100)

	eng.HandleBinLanded(landing("HPL5-NODE", 9107, 7, 64, "PART-HPL5", 41))

	rt := runtimeOf(t, eng, nodeID)
	if rt.ActiveBinID == nil || *rt.ActiveBinID != 9107 || rt.RemainingUOPCached != 64 || rt.ActiveBinEpoch != 7 {
		t.Errorf("seat = bin %v count %d epoch %d, want 9107/64/7", rt.ActiveBinID, rt.RemainingUOPCached, rt.ActiveBinEpoch)
	}
}

// TestH5_ADeliveredFromALandingCoreBindsNothing — H.5: a delivered whose Core
// announced the landing (LandingSent) closes the order, binds nothing and
// writes no count, even onto an empty seat.
func TestH5_ADeliveredFromALandingCoreBindsNothing(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	_, nodeID, _, _ := emptySeat(t, eng, "HPL6", "PART-HPL6", 100)

	orderID, err := eng.db.CreateOrder("uuid-hpl6", orders.TypeRetrieve, &nodeID, false, 1, "HPL6-NODE", "", "", "", false, "", "", "")
	testutil.MustNoErr(t, err, "create order")
	bid, n := int64(9108), 80
	testutil.MustNoErr(t, eng.db.UpdateOrderBinID(orderID, &bid), "order bin")
	eng.Events.Emit(Event{Type: EventOrderDelivered, Payload: OrderDeliveredEvent{
		OrderID: orderID, OrderUUID: "uuid-hpl6", OrderType: orders.TypeRetrieve,
		ProcessNodeID: &nodeID, BinID: &bid, BinUOP: &n, BinEpoch: 3, LandingSent: true,
	}})

	if rt := runtimeOf(t, eng, nodeID); rt.ActiveBinID != nil || rt.RemainingUOPCached != 0 {
		t.Errorf("seat = bin %v count %d, want empty/0: the delivered bound (H.5)", rt.ActiveBinID, rt.RemainingUOPCached)
	}
}

// TestH7_AnOlderCoresDeliveredNeverDisplacesADifferentCarrier — H.7, a new
// Edge with an older Core: the delivered's carrier fields bind through the
// landing verb with no placement stamp, into an empty slot or onto the same
// bin only. A different carrier bound at the seat stays.
func TestH7_AnOlderCoresDeliveredNeverDisplacesADifferentCarrier(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	_, nodeID, _, _ := emptySeat(t, eng, "HPL7", "PART-HPL7", 100)

	eng.HandleBinLanded(landing("HPL7-NODE", 9109, 2, 70, "PART-HPL7", 51))
	deliver(t, eng, nodeID, "uuid-hpl7", "HPL7-NODE", 9110, 1, 100)

	rt := runtimeOf(t, eng, nodeID)
	if rt.ActiveBinID == nil || *rt.ActiveBinID != 9109 || rt.RemainingUOPCached != 70 {
		t.Errorf("seat = bin %v count %d, want 9109/70: an older Core's delivered displaced the carrier on the seat",
			rt.ActiveBinID, rt.RemainingUOPCached)
	}

	// Onto an empty seat it binds, as it always did.
	_, emptyID, _, _ := emptySeat(t, eng, "HPL8", "PART-HPL8", 100)
	deliver(t, eng, emptyID, "uuid-hpl8", "HPL8-NODE", 9111, 1, 45)
	if rt := runtimeOf(t, eng, emptyID); rt.ActiveBinID == nil || *rt.ActiveBinID != 9111 || rt.RemainingUOPCached != 45 {
		t.Errorf("empty seat = bin %v count %d, want 9111/45", rt.ActiveBinID, rt.RemainingUOPCached)
	}
}
