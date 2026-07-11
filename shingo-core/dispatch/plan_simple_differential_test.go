//go:build docker

package dispatch

import (
	"reflect"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/fleet/seerrds"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// assertFleetEquivalent pins the SEMANTIC equivalence a differential needs: the
// plan's stepsToBlocks output has the same node sequence + SEER task kinds as the
// fleet request the unified tail sent (pickup→JackLoad@blocks[0].Location,
// dropoff→JackUnload@blocks[1].Location), and a no-wait simple order is created
// Complete=true. NOT a struct-equal on the request (blockId/goodsId differences
// are cosmetic — only location + binTask drive SEER).
func assertFleetEquivalent(t *testing.T, plan []resolvedStep, req fleet.CreateOrderRequest) {
	t.Helper()
	if !req.Complete {
		t.Fatalf("simple no-wait dispatch must create Complete=true, got false")
	}
	blocks := stepsToBlocks("v-diff", plan, 0)
	if len(blocks) != 2 {
		t.Fatalf("plan produced %d blocks, want 2", len(blocks))
	}
	if len(req.Blocks) != 2 {
		t.Fatalf("fleet request produced %d blocks, want 2", len(req.Blocks))
	}
	if blocks[0].Location != req.Blocks[0].Location || blocks[0].BinTask != seerrds.BinTaskForAction(protocol.ActionPickup) {
		t.Fatalf("pickup block = plan{loc=%s task=%s} fleet{loc=%s task=%s}, want loc match + JackLoad",
			blocks[0].Location, blocks[0].BinTask, req.Blocks[0].Location, req.Blocks[0].BinTask)
	}
	if blocks[1].Location != req.Blocks[1].Location || blocks[1].BinTask != seerrds.BinTaskForAction(protocol.ActionDropoff) {
		t.Fatalf("dropoff block = plan{loc=%s task=%s} fleet{loc=%s task=%s}, want loc match + JackUnload",
			blocks[1].Location, blocks[1].BinTask, req.Blocks[1].Location, req.Blocks[1].BinTask)
	}
}

// TestStage2_RetrieveEmptyPlanDifferential_Dispatch: the empty-carrier intent
// that was OrderType==RetrieveEmpty is preserved as step DATA (Plan[0].Empty),
// and the plan is otherwise fleet-equivalent to the transport tail.
func TestStage2_RetrieveEmptyPlanDifferential_Dispatch(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	srcNode := &nodes.Node{Name: "RE-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(srcNode), "create src node")
	empty := &bins.Bin{BinTypeID: bt.ID, Label: "RE-EMPTY", NodeID: &srcNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(empty), "create empty bin")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	order := &orders.Order{
		EdgeUUID: "re-diff-1", StationID: "ST", OrderType: OrderTypeRetrieveEmpty, Status: StatusPending, SourceIntent: SourceIntentEmpty,
		Quantity: 1, PayloadCode: bp.Code, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planTransport(order, testEnvelope(), bp.Code)
	if perr != nil {
		t.Fatalf("planTransport: %s", perr.Detail)
	}
	// The claim-move to the scanner: planTransport emits the shadow plan and QUEUES
	// — the scanner is the single claimer, so intake no longer claims or dispatches
	// inline.
	if !result.Queued {
		t.Fatal("retrieve_empty must queue for the scanner (claim-moved), not dispatch inline")
	}
	want := []resolvedStep{
		{Action: protocol.ActionPickup, Node: result.SourceNode.Name, Empty: true},
		{Action: protocol.ActionDropoff, Node: result.DestNode.Name},
	}
	if !reflect.DeepEqual(result.Plan, want) {
		t.Fatalf("emitted shadow plan = %+v, want %+v", result.Plan, want)
	}
	if !result.Plan[0].Empty {
		t.Fatal("retrieve_empty pickup must carry Empty=true (intent survives as step data, not OrderType)")
	}
	// The scanner (mirrored) claims the empty and dispatches. Assert the claim-moved
	// dispatch claims the right bin and is fleet-equivalent to the shadow plan.
	dispatched := dispatchSimpleViaScanner(t, d, db, order.EdgeUUID)
	if dispatched.BinID == nil || *dispatched.BinID != empty.ID {
		t.Fatalf("scanner claimed bin = %v, want empty %d", dispatched.BinID, empty.ID)
	}
	reqs := backend.CreateRequests()
	if len(reqs) != 1 {
		t.Fatalf("claim-moved dispatch produced %d create requests, want 1", len(reqs))
	}
	assertFleetEquivalent(t, result.Plan, reqs[0])
}

// TestStage2_MovePlanDifferential_Dispatch: happy-path move — plan emitted,
// fleet-equivalent.
func TestStage2_MovePlanDifferential_Dispatch(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	srcNode := &nodes.Node{Name: "MV-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(srcNode), "create src")
	dstNode := &nodes.Node{Name: "MV-DST", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dstNode), "create dst")
	mvBin := &bins.Bin{BinTypeID: bt.ID, Label: "MV-BIN", NodeID: &srcNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(mvBin), "create bin")
	testutil.MustNoErr(t, db.SetBinManifest(mvBin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, bp.Code, 40), "manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(mvBin.ID, ""), "confirm")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	order := &orders.Order{
		EdgeUUID: "mv-diff-1", StationID: "ST", OrderType: OrderTypeMove, Status: StatusPending, SourceIntent: SourceIntentLocal,
		Quantity: 1, PayloadCode: bp.Code, SourceNode: srcNode.Name, DeliveryNode: dstNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planTransport(order, testEnvelope(), bp.Code)
	if perr != nil {
		t.Fatalf("planTransport: %s", perr.Detail)
	}
	// The claim-move to the scanner: planTransport emits the shadow plan and QUEUES.
	if !result.Queued {
		t.Fatal("move with source + free dest must queue for the scanner (claim-moved)")
	}
	want := []resolvedStep{
		{Action: protocol.ActionPickup, Node: result.SourceNode.Name},
		{Action: protocol.ActionDropoff, Node: dstNode.Name},
	}
	if !reflect.DeepEqual(result.Plan, want) {
		t.Fatalf("emitted shadow plan = %+v, want %+v", result.Plan, want)
	}
	// The scanner (mirrored) claims + dispatches; assert fleet-equivalent to the plan.
	dispatched := dispatchSimpleViaScanner(t, d, db, order.EdgeUUID)
	if dispatched.BinID == nil || *dispatched.BinID != mvBin.ID {
		t.Fatalf("scanner claimed bin = %v, want move bin %d", dispatched.BinID, mvBin.ID)
	}
	reqs := backend.CreateRequests()
	if len(reqs) != 1 {
		t.Fatalf("claim-moved dispatch produced %d create requests, want 1", len(reqs))
	}
	assertFleetEquivalent(t, result.Plan, reqs[0])
}

// TestStage2_MovePlanDifferential_SameNode: a same-node move is a terminal
// validation error (planning_service.go same-node guard) — no PlanningResult,
// no plan.
func TestStage2_MovePlanDifferential_SameNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	order := &orders.Order{
		EdgeUUID: "mv-diff-2", StationID: "ST", OrderType: OrderTypeMove, Status: StatusPending, SourceIntent: SourceIntentLocal,
		Quantity: 1, PayloadCode: bp.Code, SourceNode: lineNode.Name, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planTransport(order, testEnvelope(), bp.Code)
	if perr == nil {
		t.Fatal("same-node move must fail (source == dest)")
	}
	if perr.Code != codeSameNode {
		t.Fatalf("error code = %s, want codeSameNode", perr.Code)
	}
	if result != nil {
		t.Fatalf("terminal same-node move must carry no PlanningResult, got %+v", result)
	}
}

// TestStage2_MovePlanDifferential_NGRPDest: a move to a synthetic NGRP resolves
// the destination to a concrete child at dispatch — the plan's dropoff must be
// the CONCRETE child, never the group name.
func TestStage2_MovePlanDifferential_NGRPDest(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}
	grp := &nodes.Node{Name: "MV-GRP", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create NGRP")
	grp, _ = db.GetNode(grp.ID)
	child := &nodes.Node{Name: "MV-DC-01", ParentID: &grp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(child), "create NGRP child")

	srcNode := &nodes.Node{Name: "MV2-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(srcNode), "create src")
	mvBin := &bins.Bin{BinTypeID: bt.ID, Label: "MV2-BIN", NodeID: &srcNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(mvBin), "create bin")
	testutil.MustNoErr(t, db.SetBinManifest(mvBin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, bp.Code, 40), "manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(mvBin.ID, ""), "confirm")

	// planMove's synthetic-dest resolution needs a resolver wired.
	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", &DefaultResolver{DB: db})

	order := &orders.Order{
		EdgeUUID: "mv-diff-3", StationID: "ST", OrderType: OrderTypeMove, Status: StatusPending, SourceIntent: SourceIntentLocal,
		Quantity: 1, PayloadCode: bp.Code, SourceNode: srcNode.Name, DeliveryNode: grp.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planTransport(order, testEnvelope(), bp.Code)
	if perr != nil {
		t.Fatalf("planTransport (NGRP dest): %s", perr.Detail)
	}
	// The claim-move to the scanner: planTransport queues. The move's synthetic-
	// NGRP-dest resolution stays at intake (the scanner dispatches to
	// order.DeliveryNode verbatim), so the concrete child must already be on the
	// shadow plan + the order's delivery node.
	if !result.Queued {
		t.Fatal("move to an NGRP with a free child must queue for the scanner (claim-moved)")
	}
	if result.DestNode.Name != child.Name {
		t.Fatalf("dest resolved to %s, want concrete child %s", result.DestNode.Name, child.Name)
	}
	if len(result.Plan) != 2 || result.Plan[1].Node != child.Name {
		t.Fatalf("plan dropoff = %+v, want concrete child %s (never the NGRP)", result.Plan, child.Name)
	}
	// The resolved concrete dest must be persisted so the scanner dispatches to the
	// child, not the group.
	persisted, err := db.GetOrderByUUID(order.EdgeUUID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if persisted.DeliveryNode != child.Name {
		t.Fatalf("persisted delivery_node = %s, want concrete child %s", persisted.DeliveryNode, child.Name)
	}
}

// TestStage2_MovePlanDifferential_MissingSource: a move with no source_node is a
// terminal validation error — no PlanningResult, no plan.
func TestStage2_MovePlanDifferential_MissingSource(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	order := &orders.Order{
		EdgeUUID: "mv-diff-4", StationID: "ST", OrderType: OrderTypeMove, Status: StatusPending, SourceIntent: SourceIntentLocal,
		Quantity: 1, PayloadCode: bp.Code, SourceNode: "", DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planTransport(order, testEnvelope(), bp.Code)
	if perr == nil {
		t.Fatal("move with no source_node must fail")
	}
	if perr.Code != codeMissingSource {
		t.Fatalf("error code = %s, want codeMissingSource", perr.Code)
	}
	if result != nil {
		t.Fatalf("terminal missing-source move must carry no PlanningResult, got %+v", result)
	}
}
