package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// THE CLASS FIX, END TO END.
//
// The Edge could not answer "what is this carrier": it stores active_bin_id, an
// opaque Core id, and has no bins table. So it inferred the answer from the
// requested style's claim, which is right only while the requested and resident
// identities agree — and a changeover that moves a cell on while a carrier is
// still standing on it is exactly when they do not. Core was holding the fact
// the whole time and dropped it on the same line it kept the count and the epoch.
func TestResidentEvacDest_PrefersTheCarriersOwnPayload(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	nodeID, _, newClaimID := residentClaimFixture(t, db)

	// The cell physically holds the OUTGOING style's part. Core told us so on
	// the delivery envelope. active_claim_id is deliberately NOT set: this must
	// work off the payload alone, or it is still the old heuristic.
	if err := db.SetProcessNodeRuntimeLinesidePayload(nodeID, "PART-OLD", true, string(domain.CarrierFromDelivery)); err != nil {
		t.Fatalf("record resident payload: %v", err)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if rt.ActiveClaimID != nil {
		t.Fatalf("fixture: active_claim_id should be unset for this test, got %v", rt.ActiveClaimID)
	}
	requested, err := db.GetStyleNodeClaim(newClaimID)
	if err != nil {
		t.Fatalf("read requested claim: %v", err)
	}

	if got := e.residentEvacDest(rt, requested); got != "HOME-OLD" {
		t.Fatalf("residentEvacDest = %q, want HOME-OLD. The carrier's own payload says where it "+
			"lives; the requested claim points at HOME-NEW, which is the incoming style's "+
			"dedicated home and is SPR 2026-09-02 exactly.", got)
	}
}

// The ordinary case: lineside and requested agree, so there is nothing to
// override and every routine swap in the plant builds byte-identical orders.
func TestResidentEvacDest_MatchingPayloadIsNoOverride(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	nodeID, _, newClaimID := residentClaimFixture(t, db)

	if err := db.SetProcessNodeRuntimeLinesidePayload(nodeID, "PART-NEW", true, string(domain.CarrierFromDelivery)); err != nil {
		t.Fatalf("record resident payload: %v", err)
	}
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	requested, _ := db.GetStyleNodeClaim(newClaimID)

	if got := e.residentEvacDest(rt, requested); got != "" {
		t.Errorf("residentEvacDest = %q, want \"\" — the carrier is the requested style, so an "+
			"override would be a no-op with a chance of being wrong", got)
	}
}

// No recorded payload means an older Core, or a bin bound by a path that
// carries no envelope. Fail open: the claim-id heuristic below is still there,
// and beyond that today's behaviour is unchanged.
func TestResidentEvacDest_NoRecordedPayloadFallsThrough(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	nodeID, _, newClaimID := residentClaimFixture(t, db)

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	requested, _ := db.GetStyleNodeClaim(newClaimID)
	if rt.LinesidePayloadCode != "" {
		t.Fatalf("fixture: expected no recorded payload, got %q", rt.LinesidePayloadCode)
	}
	if got := e.residentEvacDest(rt, requested); got != "" {
		t.Errorf("residentEvacDest = %q, want \"\" with nothing recorded and no claim stamped", got)
	}
}

// A payload several styles claim at the same node has no single home, and
// picking one would put a carrier somewhere plausible and wrong.
func TestResidentEvacDest_AmbiguousPayloadDeclines(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	nodeID, _, newClaimID := residentClaimFixture(t, db)

	// A third style claiming the SAME node for the SAME payload as the outgoing
	// one, but sending it somewhere else.
	node, _ := db.GetProcessNode(nodeID)
	thirdStyle, err := db.CreateStyle("STYLE-THIRD", "third", node.ProcessID)
	if err != nil {
		t.Fatalf("create third style: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: thirdStyle, CoreNodeName: "TEST-NODE", Role: "consume",
		SwapMode: "two_robot", PayloadCode: "PART-OLD", UOPCapacity: 100,
		InboundSource: "HOME-THIRD", InboundStaging: "STAGE-IN",
		OutboundStaging: "STAGE-OUT", OutboundDestination: "HOME-THIRD",
	}); err != nil {
		t.Fatalf("upsert third claim: %v", err)
	}

	if err := db.SetProcessNodeRuntimeLinesidePayload(nodeID, "PART-OLD", true, string(domain.CarrierFromDelivery)); err != nil {
		t.Fatalf("record resident payload: %v", err)
	}
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	requested, _ := db.GetStyleNodeClaim(newClaimID)

	if got := e.residentEvacDest(rt, requested); got != "" {
		t.Errorf("residentEvacDest = %q, want \"\" — two styles claim this payload at this node "+
			"and send it to different homes, so there is no answer to give", got)
	}
}

// The delivery path must actually record what Core told it, or the whole chain
// is a field nobody fills in. This is the half that runs in the plant every
// time a bin lands.
func TestDelivered_RecordsWhatCoreSaysTheCarrierIs(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "RESID", PayloadCode: "PART-REQUESTED", UOPCapacity: 300, InitialUOP: 0,
	})
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil), "empty slot pre-delivery")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")

	const uuid = "uuid-resid"
	const binID int64 = 707
	orderID, err := db.CreateOrder(uuid, orders.TypeRetrieve, &nodeID, false, 1,
		node.CoreNodeName, "", "", "", false, "PART-REQUESTED")
	testutil.MustNoErr(t, err, "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusInTransit)), "set in_transit")

	eng := testEngineWithOrderBridge(t, db)

	// Core says the carrier that landed is a DIFFERENT part from the one this
	// node's claim names. That disagreement is the entire subject.
	bid, uop := binID, 250
	resident := "PART-RESIDENT"
	testutil.MustNoErr(t,
		eng.orderMgr.HandleDeliveredWithExpiry(uuid, "delivery", nil, &bid, &uop, &resident, 4, node.CoreNodeName, ""),
		"handle delivered")

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime")
	if rt.LinesidePayloadCode != "PART-RESIDENT" {
		t.Fatalf("LinesidePayloadCode = %q, want PART-RESIDENT. Core read it off the same bin row "+
			"as the count and the epoch; if it is not recorded here the Edge is back to inferring "+
			"the carrier's identity from the requested style's claim.", rt.LinesidePayloadCode)
	}
}

// An older Core sends no payload. Recording a blank is right — a stale identity
// held over would be a confident wrong answer about the carrier that just
// arrived, and the readers of this field fail open on an empty one.
func TestDelivered_NoPayloadFromCoreLeavesNoStaleIdentity(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "RESID-OLD", PayloadCode: "PART-REQ2", UOPCapacity: 300, InitialUOP: 0,
	})
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(nodeID, "PART-STALE", true, string(domain.CarrierFromDelivery)), "seed a stale identity")
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil), "empty slot pre-delivery")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")

	const uuid = "uuid-resid-old"
	orderID, err := db.CreateOrder(uuid, orders.TypeRetrieve, &nodeID, false, 1,
		node.CoreNodeName, "", "", "", false, "PART-REQ2")
	testutil.MustNoErr(t, err, "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusInTransit)), "set in_transit")

	eng := testEngineWithOrderBridge(t, db)
	bid, uop := int64(708), 100
	testutil.MustNoErr(t,
		eng.orderMgr.HandleDeliveredWithExpiry(uuid, "delivery", nil, &bid, &uop, nil, 4, node.CoreNodeName, ""),
		"handle delivered")

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime")
	if rt.LinesidePayloadCode != "" {
		t.Errorf("LinesidePayloadCode = %q, want empty. The previous carrier's identity must not "+
			"survive a delivery that could not name the new one.", rt.LinesidePayloadCode)
	}
}

// THE MISMATCH, CLOSED AT ITS SOURCE.
//
// The consume/produce tick stamped its bin delta with the CLAIM's payload.
// The claim follows the process's active style, so a changeover over a node
// still holding the outgoing carrier moved it while the bin stayed put — and
// every tick then reached Core naming the incoming part against a bin holding
// the outgoing one. Core refused them as payload_mismatch_dropped and the count
// went nowhere. That refusal is the 36-minute warning on the SMN_029 carrier,
// logged from the wrong end of the wire.
func TestBinAtNode_StampsTheBinsPayloadNotTheClaims(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "TICK-ID", PayloadCode: "PART-REQUESTED", UOPCapacity: 100, InitialUOP: 50,
	})
	bin := int64(9500)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, 50), "bind the bin")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(nodeID, "PART-RESIDENT", true, string(domain.CarrierFromDelivery)),
		"Core said what the carrier is")

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	claim, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "read claim")

	eng := testEngine(t, db)
	gotBin, gotPayload, _ := eng.binAtNode(rt, claim)
	if gotBin != bin {
		t.Errorf("bin = %d, want %d", gotBin, bin)
	}
	if gotPayload != "PART-RESIDENT" {
		t.Fatalf("payload = %q, want PART-RESIDENT. Core validates the envelope against the BIN "+
			"row, so stamping the claim's %q is a delta Core is obliged to refuse.",
			gotPayload, claim.PayloadCode)
	}
}

// Nothing recorded — an older Core, or a node not delivered to since the
// upgrade — falls back to the claim, which is today's behaviour and the same
// value it would have sent anyway.
func TestBinAtNode_FallsBackToTheClaimWhenNothingIsRecorded(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "TICK-FB", PayloadCode: "PART-CLAIMED", UOPCapacity: 100, InitialUOP: 50,
	})
	bin := int64(9501)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, 50), "bind the bin")

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	claim, _ := db.GetStyleNodeClaim(claimID)
	eng := testEngine(t, db)

	if _, gotPayload, _ := eng.binAtNode(rt, claim); gotPayload != "PART-CLAIMED" {
		t.Errorf("payload = %q, want the claim's PART-CLAIMED as the fallback", gotPayload)
	}
}
