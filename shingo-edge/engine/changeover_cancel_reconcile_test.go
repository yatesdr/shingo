package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// physicalBinCoreServer returns an httptest server answering BinAtLineside
// with a single bin at coreNode (used to assert the reconcile rebinds the
// active-bin pointer to Core's physical truth).
func physicalBinCoreServer(coreNode string, binID int64, uop int, epoch int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]NodeBinInfo{{
			NodeName:     coreNode,
			BinID:        binID,
			UOPRemaining: uop,
			DeltaEpoch:   epoch,
			Occupied:     true,
		}})
	}))
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]NodeBinInfo{{
			NodeName:     coreNode,
			BinID:        physicalBinID,
			UOPRemaining: physicalUOP,
			DeltaEpoch:   physicalEpoch,
			Occupied:     true,
		}})
	}))
	defer srv.Close()

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.coreClient = NewCoreClient(srv.URL)

	_, _ = startChangeover(t, eng, db, processID, toStyleID)

	// Stamp a STALE active bin (the old bin the evac was removing, left
	// dangling). Thread the existing claim through unchanged.
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	staleBin := int64(999)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, rt.ActiveClaimID, &staleBin, 5), "seed stale active bin")

	testutil.MustNoErr(t, eng.CancelProcessChangeover(processID), "cancel")

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
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)

	node, _ := db.GetProcessNode(nodeID)
	const physicalBinID = int64(555)
	const physicalUOP = 77
	const physicalEpoch = int64(9)
	srv := physicalBinCoreServer(node.CoreNodeName, physicalBinID, physicalUOP, physicalEpoch)
	defer srv.Close()

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.coreClient = NewCoreClient(srv.URL)

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

	post, _ := db.GetProcessNodeRuntime(nodeID)
	if post.ActiveBinID == nil || *post.ActiveBinID != physicalBinID {
		t.Errorf("post-abort ActiveBinID = %v, want %d (bail branch must rebind to the physical bin, not stale 999)",
			post.ActiveBinID, physicalBinID)
	}
	if post.RemainingUOPCached != physicalUOP {
		t.Errorf("post-abort RemainingUOPCached = %d, want %d", post.RemainingUOPCached, physicalUOP)
	}
}

// TestRegression_ConfirmedCompletionDoesNotReconcile is the negative case:
// a normal CONFIRMED completion must NOT trip the bail-branch reconcile
// (which would clobber a just-finalized count with Core's lineside snapshot).
// The reconcile's signature values (bin 555 / uop 77) must not appear.
func TestRegression_ConfirmedCompletionDoesNotReconcile(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)

	node, _ := db.GetProcessNode(nodeID)
	srv := physicalBinCoreServer(node.CoreNodeName, 555, 77, 9)
	defer srv.Close()

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.coreClient = NewCoreClient(srv.URL)

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

	post, _ := db.GetProcessNodeRuntime(nodeID)
	if post.ActiveBinID != nil && *post.ActiveBinID == 555 {
		t.Errorf("ActiveBinID = 555: the reconcile fired on a confirmed completion (should not)")
	}
	if post.RemainingUOPCached == 77 {
		t.Errorf("RemainingUOPCached = 77: the reconcile fired on a confirmed completion (should not)")
	}
}
