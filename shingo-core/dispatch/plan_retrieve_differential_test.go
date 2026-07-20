//go:build docker

package dispatch

import (
	"reflect"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/seerrds"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/orders"
)

// TestStage1_RetrievePlanDifferential_Dispatch pins that the plan emitted for a
// retrieve at intake is behavior-neutral vs the unified create tail: same
// source bin, same node sequence + task kinds, Complete=true. The comparison is
// SEMANTIC (block locations/tasks vs the fleet request's blocks), never a
// struct-equal on the fleet request — blockId/goodsId differences are cosmetic
// (only location + binTask drive SEER).
func TestStage1_RetrievePlanDifferential_Dispatch(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	// A full source bin at storage; the delivery (line) node is empty.
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})
	src := &bins.Bin{BinTypeID: bt.ID, Label: "RB-DIFF-1", NodeID: &storageNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(src), "create source bin")
	testutil.MustNoErr(t, db.SetBinManifest(src.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, bp.Code, 40), "set manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(src.ID, ""), "confirm manifest")

	order := &orders.Order{
		EdgeUUID: "ret-diff-1", StationID: "ST", OrderType: OrderTypeRetrieve, Status: StatusPending,
		Quantity: 1, PayloadCode: bp.Code, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	// Intake planning — emits the shadow plan and QUEUES (the claim-move to the
	// scanner: the scanner is the single claimer, so intake resolves + queues but
	// does not claim).
	result, perr := d.planner.planTransport(order, testEnvelope(), bp.Code)
	if perr != nil {
		t.Fatalf("planTransport: %s", perr.Detail)
	}
	if !result.Queued {
		t.Fatal("retrieve with an available source must queue for the scanner (claim-moved)")
	}
	if result.SourceNode == nil || result.DestNode == nil {
		t.Fatal("shadow plan result must carry source + dest nodes")
	}

	// The emitted shadow plan is the canonical two-step retrieve.
	wantPlan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: result.SourceNode.Name},
		{Action: protocol.ActionDropoff, Node: result.DestNode.Name},
	}
	if !reflect.DeepEqual(result.Plan, wantPlan) {
		t.Fatalf("emitted shadow plan = %+v, want %+v", result.Plan, wantPlan)
	}

	// Differential: the scanner (mirrored) claims the staged bin and dispatches;
	// capture the unified create request and assert the plan's blocks are fleet-
	// equivalent (same node sequence + SEER task kinds) and Complete=true (no-wait).
	dispatched := dispatchSimpleViaScanner(t, d, db, order.EdgeUUID)
	if dispatched.BinID == nil || *dispatched.BinID != src.ID {
		t.Fatalf("scanner claimed source bin = %v, want %d", dispatched.BinID, src.ID)
	}
	reqs := backend.CreateRequests()
	if len(reqs) != 1 {
		t.Fatalf("claim-moved dispatch produced %d create requests, want 1", len(reqs))
	}
	req := reqs[0]
	if !req.Complete {
		t.Fatal("simple no-wait dispatch must create Complete=true, got false")
	}

	blocks := stepsToBlocks("v-diff", result.Plan, 0, nil)
	if len(blocks) != 2 {
		t.Fatalf("plan produced %d blocks, want 2", len(blocks))
	}
	if len(req.Blocks) != 2 {
		t.Fatalf("fleet request produced %d blocks, want 2", len(req.Blocks))
	}
	if blocks[0].Location != req.Blocks[0].Location || blocks[0].BinTask != seerrds.BinTaskForAction(protocol.ActionPickup) {
		t.Fatalf("plan pickup block = {loc=%s task=%s}, fleet = {loc=%s task=%s}, want loc match + JackLoad",
			blocks[0].Location, blocks[0].BinTask, req.Blocks[0].Location, req.Blocks[0].BinTask)
	}
	if blocks[1].Location != req.Blocks[1].Location || blocks[1].BinTask != seerrds.BinTaskForAction(protocol.ActionDropoff) {
		t.Fatalf("plan dropoff block = {loc=%s task=%s}, fleet = {loc=%s task=%s}, want loc match + JackUnload",
			blocks[1].Location, blocks[1].BinTask, req.Blocks[1].Location, req.Blocks[1].BinTask)
	}
}

// TestStage1_RetrievePlanDifferential_GatedNoPlan pins the non-dispatch
// disposition: a retrieve blocked by the dropoff-capacity gate queues (unchanged)
// and emits NO plan (a plan is emitted only for a dispatching order).
func TestStage1_RetrievePlanDifferential_GatedNoPlan(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Occupy the delivery node so the capacity gate fires.
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})
	occ := &bins.Bin{BinTypeID: bt.ID, Label: "OCC-DIFF", NodeID: &lineNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(occ), "create occupying bin")
	// A source bin exists too, so capacity is the only blocker.
	src := &bins.Bin{BinTypeID: bt.ID, Label: "RB-DIFF-2", NodeID: &storageNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(src), "create source bin")
	testutil.MustNoErr(t, db.SetBinManifest(src.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, bp.Code, 40), "set manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(src.ID, ""), "confirm manifest")

	order := &orders.Order{
		EdgeUUID: "ret-diff-2", StationID: "ST", OrderType: OrderTypeRetrieve, Status: StatusPending,
		Quantity: 1, PayloadCode: bp.Code, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planTransport(order, testEnvelope(), bp.Code)
	if perr != nil {
		t.Fatalf("planTransport: %s", perr.Detail)
	}
	if !result.Queued {
		t.Fatal("retrieve to an occupied destination must queue")
	}
	if result.Plan != nil {
		t.Fatalf("no plan must be emitted for a queued (non-dispatch) retrieve, got %+v", result.Plan)
	}
	if reloaded, err := db.GetOrder(order.ID); err == nil && reloaded.QueueReason == "" {
		t.Error("queued retrieve must carry a queue_reason (disposition unchanged)")
	}
}
