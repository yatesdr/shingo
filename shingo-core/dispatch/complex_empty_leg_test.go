//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestReserveConfirm_EmptyLegClaimsEmptyCarrier is the produce-node fix, ported to
// the reserve/confirm split: a complex swap stores the produced full and
// fetches an EMPTY carrier to refill the press. The empty pickup leg (step.Empty)
// must reserve+claim an empty carrier, NOT a payload-matching full sitting in the
// same source — the Hopkinsville bug where the press kept being handed full bins.
//
// Source node holds BOTH a full PART-A bin and an empty carrier. The empty-leg
// filter lives in reserveComplexPlan.findAvailableForNeed (emptyBinsOnly), so the
// reserve selects the empty and confirm claims exactly it.
func TestReserveConfirm_EmptyLegClaimsEmptyCarrier(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storeNode, lineNode, bp := setupTestData(t, db)

	// Supermarket the press pulls empties from — holds a full AND an empty.
	src := &nodes.Node{Name: "EMPTY-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "create empty-source node")

	fullAtSrc := &bins.Bin{BinTypeID: 1, Label: "SRC-FULL", NodeID: &src.ID, Status: "available", PayloadCode: bp.Code}
	testutil.MustNoErr(t, db.CreateBin(fullAtSrc), "create full bin at source")
	emptyAtSrc := &bins.Bin{BinTypeID: 1, Label: "SRC-EMPTY", NodeID: &src.ID, Status: "available", PayloadCode: ""}
	testutil.MustNoErr(t, db.CreateBin(emptyAtSrc), "create empty carrier at source")

	// The produced full sitting on the line, picked up to be stored.
	lineFull := &bins.Bin{BinTypeID: 1, Label: "LINE-FULL", NodeID: &lineNode.ID, Status: "available", PayloadCode: bp.Code}
	testutil.MustNoErr(t, db.CreateBin(lineFull), "create line full bin")

	order := &orders.Order{
		EdgeUUID:     "uuid-empty-leg",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     1,
		SourceNode:   lineNode.Name,
		ProcessNode:  lineNode.Name,
		DeliveryNode: lineNode.Name,
		PayloadCode:  bp.Code,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// pickup line full → store at storeNode → pickup EMPTY at src → deliver to line.
	steps := []resolvedStep{
		{Action: "wait", Node: lineNode.Name},
		{Action: "pickup", Node: lineNode.Name},
		{Action: "dropoff", Node: storeNode.Name},
		{Action: "pickup", Node: src.Name, Empty: true},
		{Action: "dropoff", Node: lineNode.Name},
	}
	plan := BuildComplexPlan(steps, d.snapshotPickupBins(steps), bp.Code, order.ProcessNode)
	assigned, outcome, rerr := d.allocator.reserveComplexPlan(order, plan)
	if rerr != nil {
		t.Fatalf("reserveComplexPlan: %v", rerr)
	}
	if outcome != reserveComplete {
		t.Fatalf("reserveComplexPlan outcome = %v, want reserveComplete — line full and src empty are both available", outcome)
	}
	if cerr := d.allocator.confirmComplexPlan(order, plan, assigned); cerr != nil {
		t.Fatalf("confirmComplexPlan: %v", cerr)
	}

	// The empty carrier is claimed by this order; the full at the same source is NOT.
	gotEmpty, _ := db.GetBin(emptyAtSrc.ID)
	if gotEmpty.ClaimedBy == nil || *gotEmpty.ClaimedBy != order.ID {
		t.Errorf("empty carrier claimed_by = %v, want order %d — the empty leg must claim the empty", gotEmpty.ClaimedBy, order.ID)
	}
	gotFull, _ := db.GetBin(fullAtSrc.ID)
	if gotFull.ClaimedBy != nil {
		t.Errorf("full bin at source claimed_by = %v, want nil — the empty leg must NOT claim the full", *gotFull.ClaimedBy)
	}

	// The order_bins row for the source node points at the empty carrier.
	obs, err := db.ListOrderBins(order.ID)
	if err != nil {
		t.Fatalf("ListOrderBins: %v", err)
	}
	var srcBinID int64
	for _, ob := range obs {
		if ob.NodeName == src.Name {
			srcBinID = ob.BinID
		}
	}
	if srcBinID != emptyAtSrc.ID {
		t.Errorf("order_bins source-leg bin = %d, want empty carrier %d", srcBinID, emptyAtSrc.ID)
	}
}

// TestDispatcher_ComplexOrder_QueuesOnDryEmptyPool is the produce-side dual of
// TestDispatcher_ComplexOrder_QueuesOnSaturatedNGRP. A produce swap's empty-fetch
// leg (step.Empty) resolves a fresh EMPTY carrier from an NGRP pool. When that pool
// is momentarily dry, resolveStepNode returns "cannot resolve empty in group X".
//
// Pre-fix that error was classified ResolutionFatal and terminal-rejected at intake
// — no Core row, no retry — so a transiently-dry empty pool aborted the produce swap
// and left its supply sibling to half-strand the press (2026-07-14 sim run). The
// consume-side full dual ("no available slot / no bin of requested payload in node
// group") already queued-and-retried; this pins the symmetry: dry empty pool at
// intake → QUEUED (retryable), and it dispatches once an empty returns to the pool.
func TestDispatcher_ComplexOrder_QueuesOnDryEmptyPool(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storeNode, lineNode, bp := setupTestData(t, db)

	syntheticType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}

	// The empty-carrier supermarket the press pulls fresh carriers from — an NGRP
	// with one child slot, holding NO empty. A FULL bin sits in the slot, which the
	// empty filter (COALESCE(payload_code,'')='') deliberately skips: this proves
	// the shortfall is "no EMPTY here", not "group is empty / has no children".
	pool := &nodes.Node{Name: "EMPTY-POOL-NGRP", IsSynthetic: true, NodeTypeID: &syntheticType.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(pool), "create empty pool")
	slot := &nodes.Node{Name: "POOL-SLOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(slot), "create pool slot")
	testutil.MustNoErr(t, db.SetNodeParent(slot.ID, pool.ID), "parent slot")
	// A genuine FULL bin occupies the slot — payload set via the manifest path
	// (CreateBin does not persist a struct PayloadCode). The empty filter
	// (COALESCE(payload_code,'')='') skips it, so this proves the shortfall is
	// "no EMPTY carrier here", not "the group is empty / has no children".
	full := &bins.Bin{BinTypeID: 1, Label: "POOL-FULL", NodeID: &slot.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(full), "create full bin in pool")
	testutil.MustNoErr(t, db.SetBinManifest(full.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, bp.Code, 40), "fill the bin")
	testutil.MustNoErr(t, db.ConfirmBinManifest(full.ID, ""), "confirm full")

	// The produced full on the line — the first leg (which CAN source) picks it up.
	_ = testdb.CreateBinAtNode(t, db, bp.Code, lineNode.ID, "LINE-FULL")

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	// A produce swap shape: clear the press full → store it → fetch a fresh EMPTY
	// from the (dry) pool → stage it back at the line. The empty pickup is step 3.
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "empty-dry-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		ProcessNode: lineNode.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: storeNode.Name},
			{Action: "pickup", Node: pool.Name, Empty: true},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})

	// The order must exist at Core in queued state — NOT rejected via sendError.
	order, err := db.GetOrderByUUID("empty-dry-1")
	if err != nil {
		t.Fatalf("get order: %v — a dry empty pool must create the order QUEUED, not reject it at intake", err)
	}
	if order.Status != StatusQueued {
		t.Errorf("status = %q, want %q (a dry empty pool is sourceable-eventually — it must queue)", order.Status, StatusQueued)
	}
	if !strings.Contains(order.QueueReason, "cannot resolve empty in group") &&
		!strings.Contains(order.QueueReason, "no empty carrier in group") {
		t.Errorf("queue_reason = %q, want the empty-resolver message so the operator sees why it waits", order.QueueReason)
	}

	// An empty carrier returns to the pool → the queued order dispatches on replay
	// (the scanner drives this on EventBinUpdated; here we call it directly).
	empty := &bins.Bin{BinTypeID: 1, Label: "POOL-EMPTY", NodeID: &slot.ID, Status: "available", PayloadCode: ""}
	testutil.MustNoErr(t, db.CreateBin(empty), "add empty carrier to pool")

	if derr := d.DispatchPreparedComplex(order); derr != nil {
		t.Fatalf("DispatchPreparedComplex after an empty returned: %v — the queued order must dispatch on retry, not stay stuck", derr)
	}
	order, err = db.GetOrderByUUID("empty-dry-1")
	if err != nil {
		t.Fatalf("re-read order: %v", err)
	}
	if order.Status != StatusDispatched {
		t.Errorf("after replay status = %q, want %q (empty returned → swap proceeds)", order.Status, StatusDispatched)
	}
}
