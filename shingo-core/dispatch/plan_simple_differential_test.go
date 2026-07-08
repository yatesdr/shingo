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
// real transport request the old tail sent (pickup→JackLoad@FromLoc,
// dropoff→JackUnload@ToLoc) — NOT a struct-equal on the request.
func assertFleetEquivalent(t *testing.T, plan []resolvedStep, req fleet.TransportOrderRequest) {
	t.Helper()
	blocks := stepsToBlocks("v-diff", plan, 0)
	if len(blocks) != 2 {
		t.Fatalf("plan produced %d blocks, want 2", len(blocks))
	}
	if blocks[0].Location != req.FromLoc || blocks[0].BinTask != seerrds.BinTaskForAction(protocol.ActionPickup) {
		t.Fatalf("pickup block = {loc=%s task=%s}, want {loc=%s task=JackLoad}", blocks[0].Location, blocks[0].BinTask, req.FromLoc)
	}
	if blocks[1].Location != req.ToLoc || blocks[1].BinTask != seerrds.BinTaskForAction(protocol.ActionDropoff) {
		t.Fatalf("dropoff block = {loc=%s task=%s}, want {loc=%s task=JackUnload}", blocks[1].Location, blocks[1].BinTask, req.ToLoc)
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
		EdgeUUID: "re-diff-1", StationID: "ST", OrderType: OrderTypeRetrieveEmpty, Status: StatusPending,
		Quantity: 1, PayloadCode: bp.Code, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planRetrieveEmpty(order, testEnvelope(), bp.Code)
	if perr != nil {
		t.Fatalf("planRetrieveEmpty: %s", perr.Detail)
	}
	if result.Queued {
		t.Fatal("retrieve_empty with an available empty must dispatch, not queue")
	}
	if order.BinID == nil || *order.BinID != empty.ID {
		t.Fatalf("claimed bin = %v, want empty %d", order.BinID, empty.ID)
	}
	want := []resolvedStep{
		{Action: protocol.ActionPickup, Node: result.SourceNode.Name, Empty: true},
		{Action: protocol.ActionDropoff, Node: result.DestNode.Name},
	}
	if !reflect.DeepEqual(result.Plan, want) {
		t.Fatalf("emitted plan = %+v, want %+v", result.Plan, want)
	}
	if !result.Plan[0].Empty {
		t.Fatal("retrieve_empty pickup must carry Empty=true (intent survives as step data, not OrderType)")
	}
	d.dispatchToFleet(order, testEnvelope(), result.SourceNode, result.DestNode)
	reqs := backend.TransportRequests()
	if len(reqs) != 1 {
		t.Fatalf("old tail produced %d transport requests, want 1", len(reqs))
	}
	assertFleetEquivalent(t, result.Plan, reqs[0])
}

// TestStage2_StorePlanDifferential_Dispatch: a store dispatches with its dest
// secured through C2's claimStoreSlot; the emitted plan is fleet-equivalent.
func TestStage2_StorePlanDifferential_Dispatch(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	srcBin := &bins.Bin{BinTypeID: bt.ID, Label: "ST-SRC", NodeID: &lineNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(srcBin), "create source bin")
	testutil.MustNoErr(t, db.SetBinManifest(srcBin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, bp.Code, 40), "manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(srcBin.ID, ""), "confirm")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	order := &orders.Order{
		EdgeUUID: "st-diff-1", StationID: "ST", OrderType: OrderTypeStore, Status: StatusPending,
		Quantity: 1, PayloadCode: bp.Code, SourceNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planStore(order, testEnvelope(), bp.Code)
	if perr != nil {
		t.Fatalf("planStore: %s", perr.Detail)
	}
	if result.Queued {
		t.Fatal("store with a free storage slot must dispatch, not queue")
	}
	if result.DestNode.Name != storageNode.Name {
		t.Fatalf("dest = %s, want the STOR node %s", result.DestNode.Name, storageNode.Name)
	}
	want := []resolvedStep{
		{Action: protocol.ActionPickup, Node: lineNode.Name},
		{Action: protocol.ActionDropoff, Node: storageNode.Name},
	}
	if !reflect.DeepEqual(result.Plan, want) {
		t.Fatalf("emitted plan = %+v, want %+v", result.Plan, want)
	}
	d.dispatchToFleet(order, testEnvelope(), result.SourceNode, result.DestNode)
	reqs := backend.TransportRequests()
	if len(reqs) != 1 {
		t.Fatalf("old tail produced %d transport requests, want 1", len(reqs))
	}
	assertFleetEquivalent(t, result.Plan, reqs[0])
}

// TestStage2_StorePlanDifferential_QueuesPolitelyNoPlan: when the only storage
// slot is occupied, C2's claimStoreSlot requeues (polite wait, never terminal)
// and NO plan is emitted — the emission must not disturb the C2 behavior.
func TestStage2_StorePlanDifferential_QueuesPolitelyNoPlan(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	// Occupy the only STOR node with a same-payload bin so FindStorageDestination's
	// consolidation returns it and claimStoreSlot's occupancy guard requeues.
	occ := &bins.Bin{BinTypeID: bt.ID, Label: "ST-OCC", NodeID: &storageNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(occ), "create occupying bin")
	testutil.MustNoErr(t, db.SetBinManifest(occ.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, bp.Code, 40), "manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(occ.ID, ""), "confirm")

	srcBin := &bins.Bin{BinTypeID: bt.ID, Label: "ST-SRC2", NodeID: &lineNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(srcBin), "create source bin")
	testutil.MustNoErr(t, db.SetBinManifest(srcBin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, bp.Code, 40), "manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(srcBin.ID, ""), "confirm")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	order := &orders.Order{
		EdgeUUID: "st-diff-2", StationID: "ST", OrderType: OrderTypeStore, Status: StatusPending,
		Quantity: 1, PayloadCode: bp.Code, SourceNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planStore(order, testEnvelope(), bp.Code)
	if perr != nil {
		t.Fatalf("planStore should queue politely, not fail: %s", perr.Detail)
	}
	if !result.Queued {
		t.Fatal("store to an occupied slot must queue politely (C2 reserve-only)")
	}
	if result.Plan != nil {
		t.Fatalf("no plan may be emitted on the queue path, got %+v", result.Plan)
	}
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
		EdgeUUID: "mv-diff-1", StationID: "ST", OrderType: OrderTypeMove, Status: StatusPending,
		Quantity: 1, PayloadCode: bp.Code, SourceNode: srcNode.Name, DeliveryNode: dstNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planMove(order, testEnvelope(), bp.Code)
	if perr != nil {
		t.Fatalf("planMove: %s", perr.Detail)
	}
	if result.Queued {
		t.Fatal("move with source + free dest must dispatch, not queue")
	}
	want := []resolvedStep{
		{Action: protocol.ActionPickup, Node: result.SourceNode.Name},
		{Action: protocol.ActionDropoff, Node: dstNode.Name},
	}
	if !reflect.DeepEqual(result.Plan, want) {
		t.Fatalf("emitted plan = %+v, want %+v", result.Plan, want)
	}
	d.dispatchToFleet(order, testEnvelope(), result.SourceNode, result.DestNode)
	reqs := backend.TransportRequests()
	if len(reqs) != 1 {
		t.Fatalf("old tail produced %d transport requests, want 1", len(reqs))
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
		EdgeUUID: "mv-diff-2", StationID: "ST", OrderType: OrderTypeMove, Status: StatusPending,
		Quantity: 1, PayloadCode: bp.Code, SourceNode: lineNode.Name, DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planMove(order, testEnvelope(), bp.Code)
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
		EdgeUUID: "mv-diff-3", StationID: "ST", OrderType: OrderTypeMove, Status: StatusPending,
		Quantity: 1, PayloadCode: bp.Code, SourceNode: srcNode.Name, DeliveryNode: grp.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planMove(order, testEnvelope(), bp.Code)
	if perr != nil {
		t.Fatalf("planMove (NGRP dest): %s", perr.Detail)
	}
	if result.Queued {
		t.Fatal("move to an NGRP with a free child must dispatch, not queue")
	}
	if result.DestNode.Name != child.Name {
		t.Fatalf("dest resolved to %s, want concrete child %s", result.DestNode.Name, child.Name)
	}
	if len(result.Plan) != 2 {
		t.Fatalf("emitted plan has %d steps, want 2", len(result.Plan))
	}
	if result.Plan[1].Node != child.Name {
		t.Fatalf("plan dropoff = %s, want concrete child %s (never the NGRP)", result.Plan[1].Node, child.Name)
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
		EdgeUUID: "mv-diff-4", StationID: "ST", OrderType: OrderTypeMove, Status: StatusPending,
		Quantity: 1, PayloadCode: bp.Code, SourceNode: "", DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	result, perr := d.planner.planMove(order, testEnvelope(), bp.Code)
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
