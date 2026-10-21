package engine

import (
	"encoding/json"
	"shingo/protocol"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
)

// answerReseats is Core answering every reseat ask on eng's outbox, as Core's
// BinService.Reseat does: for a node carrier answers with a carrier, its
// landing first (the landing leaves Core's outbox ahead of the answer), then
// the answer naming the node and whether one stands there. Each ask answered
// is taken off the outbox. It returns how many it answered.
func answerReseats(t *testing.T, eng *Engine, carrier func(node string) *protocol.BinLanded) int {
	t.Helper()
	msgs, err := eng.db.ListPendingOutbox(100)
	testutil.MustNoErr(t, err, "list outbox")
	n := 0
	for _, m := range msgs {
		if m.MsgType != protocol.SubjectEdgeAsk {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "decode ask envelope")
		var data protocol.Data
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &data), "decode ask data")
		var ask protocol.EdgeAsk
		testutil.MustNoErr(t, json.Unmarshal(data.Body, &ask), "decode ask")
		if ask.Kind != protocol.AskReseat {
			continue
		}
		var req protocol.ReseatRequest
		testutil.MustNoErr(t, json.Unmarshal(ask.Body, &req), "decode reseat")
		l := carrier(req.Node)
		if l != nil {
			eng.HandleBinLanded(*l)
		}
		body, err := json.Marshal(protocol.ReseatAnswer{Node: req.Node, Landed: l != nil})
		testutil.MustNoErr(t, err, "encode answer")
		eng.HandleEdgeAnswer(answerTo(&env), &protocol.EdgeAnswer{Kind: protocol.AskReseat, Status: protocol.AnswerOK, Body: body})
		testutil.MustNoErr(t, eng.db.AckOutbox(m.ID), "take the ask off the outbox")
		n++
	}
	return n
}

// physicalLanding is the landing Core re-announces for a bin at node, with a
// placement stamp newer than any a fixture's seat holds.
func physicalLanding(node string, binID int64, uop int, epoch int64) func(string) *protocol.BinLanded {
	return func(n string) *protocol.BinLanded {
		if n != node {
			return nil
		}
		return &protocol.BinLanded{BinID: binID, CoreNodeName: node, Count: uop, Epoch: epoch, PlacedSeq: 1 << 40}
	}
}

// TestRegression_ChangeoverCancelReconcilesActiveBinFromPhysical pins the
// Springfield 2026-06-02 cancel-path fix. Cancelling a changeover used to clear
// only the runtime ORDER refs and leave active_bin_id pointing at the old bin —
// so after an evac moved (or an operator manually swapped) the bin, consume
// ticks kept draining the stale bin. The cancel now re-resolves each affected
// node against Core's physical bin-at-node and rebinds the pointer (with the
// authoritative count + epoch).
func TestRegression_ChangeoverCancelReconcilesActiveBinFromPhysical(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)

	node, _ := db.GetProcessNode(nodeID)
	coreNode := node.CoreNodeName

	// Core reports a DIFFERENT bin physically at the slot than the stale
	// active pointer — the re-resolved / manually-swapped bin.
	const physicalBinID int64 = 555
	const physicalUOP = 77
	const physicalEpoch int64 = 9
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.setAskKinds([]string{protocol.AskReseat})

	_, _ = startChangeover(t, eng, db, processID, toStyleID)

	// Stamp a STALE active bin (the old bin the evac was removing, left
	// dangling). Thread the existing claim through unchanged.
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	staleBin := int64(999)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, rt.ActiveClaimID, &staleBin, 5), "seed stale active bin")

	testutil.MustNoErr(t, eng.CancelProcessChangeover(processID), "cancel")
	// The cancel asks Core to reseat the node; Core re-announces the bin.
	if n := answerReseats(t, eng, physicalLanding(coreNode, physicalBinID, physicalUOP, physicalEpoch)); n == 0 {
		t.Fatal("the cancel asked Core for no reseat")
	}

	post, _ := db.GetProcessNodeRuntime(nodeID)
	if post.ActiveBinID == nil || *post.ActiveBinID != physicalBinID {
		t.Errorf("post-cancel ActiveBinID = %v, want %d (must rebind to the physical bin, not the stale 999)",
			post.ActiveBinID, physicalBinID)
	}
	if post.RemainingUOPCached != physicalUOP {
		t.Errorf("post-cancel RemainingUOPCached = %d, want %d (must reconcile to Core's authoritative count)",
			post.RemainingUOPCached, physicalUOP)
	}
}

// TestRegression_PlainAbortReconcilesActiveBinFromPhysical verifies that a plain
// abort (order cancelled/failed, no operator changeover-cancel) must reconcile
// the node's stale active-bin pointer against Core's physical truth — the same
// rebind the changeover-cancel path does, now wired into the cancelled/failed
// bail branch of handleNodeOrderCompleted.
func TestRegression_PlainAbortReconcilesActiveBinFromPhysical(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenarioMode(t, db, protocol.SwapModeTwoRobot)

	node, _ := db.GetProcessNode(nodeID)
	const physicalBinID = int64(555)
	const physicalUOP = 77
	const physicalEpoch = int64(9)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.setAskKinds([]string{protocol.AskReseat})

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.NextMaterialOrderID == nil {
		t.Fatal("scenario should have a next-material order")
	}
	orderID := *task.NextMaterialOrderID

	// Stamp a STALE active bin (the bin the aborted order strands).
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	staleBin := int64(999)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, rt.ActiveClaimID, &staleBin, 5), "seed stale active bin")

	// Abort the order and fire its terminal completion — the bail branch must
	// reconcile the stranded pointer.
	db.UpdateOrderStatus(orderID, string(orders.StatusSubmitted))
	db.UpdateOrderStatus(orderID, string(orders.StatusCancelled))
	order, _ := db.GetOrder(orderID)
	if order.ProcessNodeID == nil {
		t.Fatal("precondition: aborted order should carry a ProcessNodeID")
	}
	eng.Events.Emit(Event{Type: EventOrderCompleted, Payload: OrderCompletedEvent{
		OrderID:       orderID,
		OrderUUID:     order.UUID,
		OrderType:     order.OrderType,
		ProcessNodeID: &nodeID,
	}})
	if n := answerReseats(t, eng, physicalLanding(node.CoreNodeName, physicalBinID, physicalUOP, physicalEpoch)); n == 0 {
		t.Fatal("the abort asked Core for no reseat")
	}

	post, _ := db.GetProcessNodeRuntime(nodeID)
	if post.ActiveBinID == nil || *post.ActiveBinID != physicalBinID {
		t.Errorf("post-abort ActiveBinID = %v, want %d (bail branch must rebind to the physical bin, not stale 999)",
			post.ActiveBinID, physicalBinID)
	}
	if post.RemainingUOPCached != physicalUOP {
		t.Errorf("post-abort RemainingUOPCached = %d, want %d", post.RemainingUOPCached, physicalUOP)
	}
}

// TestRegression_TerminalOrderReleasesRuntimeOrderSlot pins the SPR ALN_005 /
// ALN_006 failure (2026-07-21): a skipped supply leg and a cancelled evac leg
// both reached terminal, but nothing cleared the node's runtime ORDER
// pointers. The cancelled branch reconciled the BIN pointer only, and skipped
// fell through the branch entirely with no cleanup at all — so the slots held
// two dead order ids until the next edge restart, and the operator screen kept
// rendering [REP] "Order in progress" against them (isReplenishing trusts a
// bare non-nil pointer without checking status).
//
// Skipped is the sharper of the two cases and is what this pins. The clear
// must also be per-slot: the surviving sibling of a two-robot swap keeps its
// pointer when its partner dies.
func TestRegression_TerminalOrderReleasesRuntimeOrderSlot(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenarioMode(t, db, protocol.SwapModeTwoRobot)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.setAskKinds([]string{protocol.AskReseat})

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if task.NextMaterialOrderID == nil {
		t.Fatal("scenario should have a next-material order")
	}
	supplyID := *task.NextMaterialOrderID

	// ALN_005's exact shape: the active slot holds the supply leg, the staged
	// slot holds its still-live evac sibling.
	sibling := int64(424242)
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &supplyID, &sibling), "seed runtime slots")

	// Core skips the supply leg — the work turned out never to be needed.
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(orders.StatusSkipped)), "skip supply leg")
	order, _ := db.GetOrder(supplyID)
	eng.Events.Emit(Event{Type: EventOrderCompleted, Payload: OrderCompletedEvent{
		OrderID:       supplyID,
		OrderUUID:     order.UUID,
		OrderType:     order.OrderType,
		ProcessNodeID: &nodeID,
	}})

	post, _ := db.GetProcessNodeRuntime(nodeID)
	if post.ActiveOrderID != nil {
		t.Errorf("active_order_id = %d after the order was skipped, want nil — a dead order must not hold the slot (this is the [REP] phantom)",
			*post.ActiveOrderID)
	}
	if post.StagedOrderID == nil || *post.StagedOrderID != sibling {
		t.Errorf("staged_order_id = %v, want %d — the live swap sibling must survive its partner's death",
			post.StagedOrderID, sibling)
	}
}

// TestRegression_ConfirmedCompletionDoesNotReconcile is the negative case:
// a normal CONFIRMED completion must NOT trip the bail-branch reconcile
// (which would clobber a just-finalized count with Core's lineside snapshot).
// The reconcile's signature values (bin 555 / uop 77) must not appear.
func TestRegression_ConfirmedCompletionDoesNotReconcile(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenarioMode(t, db, protocol.SwapModeTwoRobot)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.setAskKinds([]string{protocol.AskReseat})

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	orderID := *task.NextMaterialOrderID

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	staleBin := int64(999)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, rt.ActiveClaimID, &staleBin, 5), "seed active bin")

	// Drive to CONFIRMED (success), not cancelled/failed.
	db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed))
	order, _ := db.GetOrder(orderID)
	eng.Events.Emit(Event{Type: EventOrderCompleted, Payload: OrderCompletedEvent{
		OrderID:       orderID,
		OrderUUID:     order.UUID,
		OrderType:     order.OrderType,
		ProcessNodeID: &nodeID,
	}})
	node, err := db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("read node: %v", err)
	}
	if n := answerReseats(t, eng, physicalLanding(node.CoreNodeName, 555, 77, 9)); n != 0 {
		t.Errorf("a confirmed completion asked Core for %d reseat(s), want none", n)
	}

	post, _ := db.GetProcessNodeRuntime(nodeID)
	if post.ActiveBinID != nil && *post.ActiveBinID == 555 {
		t.Errorf("ActiveBinID = 555: the reconcile fired on a confirmed completion (should not)")
	}
	if post.RemainingUOPCached == 77 {
		t.Errorf("RemainingUOPCached = 77: the reconcile fired on a confirmed completion (should not)")
	}
}

// TestCancelReconcile_RecordsTheReboundCarriersIdentity: the reconcile rebinds
// the slot to the carrier Core says is there, and records THAT carrier's part
// through the doorway. A rebind onto a different carrier used to leave the
// previous carrier's part standing as a confident answer — and a changeover
// planned from what stands on the node, or the evacuation aim, acted on the
// wrong carrier. CHANGED (site 6 of the reads, the reseat): the rebind is
// Core's landing, so the identity is the landing's (CarrierFromLanding), where
// the HTTP reconcile recorded CarrierFromReconcile. And no Core answering the
// reseat leaves the seat as it is.
func TestCancelReconcile_RecordsTheReboundCarriersIdentity(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedPhase3SwapScenario(t, db)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "process node")
	eng := testEngine(t, db)

	// The previous carrier, known, as its landing recorded it.
	eng.recordLinesideCarrier(nodeID, node.CoreNodeName, domain.KnownCarrier("PART-OLD"), domain.CarrierFromLanding)
	before, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime before")

	// A Core that does not answer the reseat: nothing is asked, nothing moves.
	eng.reconcileActiveBinAfterCancel(nodeID)
	if n := answerReseats(t, eng, func(string) *protocol.BinLanded { return nil }); n != 0 {
		t.Fatalf("asked a Core that does not answer the reseat %d time(s)", n)
	}
	rt, rerr := db.GetProcessNodeRuntime(nodeID)
	if rerr != nil {
		t.Fatalf("read runtime: %v", rerr)
	}
	if string(rt.LinesidePayloadCode) != "PART-OLD" ||
		!sameBin(rt.ActiveBinID, before.ActiveBinID) {
		t.Errorf("no reseat answered, the seat moved: bin %v part %q", rt.ActiveBinID, rt.LinesidePayloadCode)
	}

	eng.setAskKinds([]string{protocol.AskReseat})
	eng.reconcileActiveBinAfterCancel(nodeID)
	answerReseats(t, eng, func(n string) *protocol.BinLanded {
		return &protocol.BinLanded{BinID: 555, CoreNodeName: n, Count: 40, Epoch: 3, PlacedSeq: 1 << 40,
			PayloadCode: "PART-NEW", PayloadKnown: true}
	})

	post, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime after")
	if post.ActiveBinID == nil || *post.ActiveBinID != 555 {
		t.Errorf("active bin after the reseat = %v, want 555", post.ActiveBinID)
	}
	if !post.LinesidePayloadKnown || string(post.LinesidePayloadCode) != "PART-NEW" {
		t.Errorf("identity after the rebind = %q (known %v), want PART-NEW — the rebound carrier's part, "+
			"not the previous occupant's", post.LinesidePayloadCode, post.LinesidePayloadKnown)
	}
	if post.LinesideSource != string(domain.CarrierFromLanding) {
		t.Errorf("identity source = %q, want %q", post.LinesideSource, domain.CarrierFromLanding)
	}
}

func sameBin(a, b *int64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
