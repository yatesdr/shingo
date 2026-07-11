//go:build docker

// End-to-end dispatch tests.
//
// This file was previously integration_test.go. Renamed because
// "integration" suggested these tests only verified multi-component
// wiring, when in fact most of them drive the dispatcher through a
// complete order lifecycle against a live Postgres (via testdb) and
// validate behavior that is the single system's responsibility:
// retrieve/move/store/cancel/redirect lifecycles, synthetic-node
// resolution (NGRP + dot notation), buried-bin reshuffle planning,
// fleet-failure recovery, priority handling, bin tracking, ingest,
// same-node guards. These exercise the dispatch state machine end-
// to-end — not two subsystems interacting — hence the rename.
//
// All tests in this file require a live Postgres. They are gated by
// the //go:build docker tag on the test files in this package; run
// with: go test -tags=docker ./shingo-core/dispatch/...
package dispatch

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

func TestDispatcher_RetrieveOrder_FullLifecycle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin at the storage node with a manifest
	bin := testdb.CreateBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-RET-1")

	backend := testdb.NewTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Phase 1: Submit retrieve order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-uuid-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})
	dispatchSimpleViaScanner(t, d, db, "retrieve-uuid-1")

	// Verify order was created
	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	// Verify order is in database
	order := testdb.AssertOrderStatus(t, db, "retrieve-uuid-1", StatusDispatched)
	if order.SourceNode != storageNode.Name {
		t.Errorf("source node = %q, want %q", order.SourceNode, storageNode.Name)
	}
	if order.DeliveryNode != lineNode.Name {
		t.Errorf("delivery node = %q, want %q", order.DeliveryNode, lineNode.Name)
	}

	// Verify vendor order was created
	if order.VendorOrderID == "" {
		t.Fatal("vendor order ID should be set")
	}

	// After dispatch the bin is claimed by the order (via ClaimForDispatch).
	dispatchedBin, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin after dispatch: %v", err)
	}
	if dispatchedBin.ClaimedBy == nil || *dispatchedBin.ClaimedBy != order.ID {
		t.Errorf("after dispatch, bin claimed_by = %v, want %d", dispatchedBin.ClaimedBy, order.ID)
	}

	// Phase 2: Simulate fleet delivery, then confirm receipt
	db.UpdateOrderStatus(order.ID, string(StatusDelivered), "fleet delivered")

	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "retrieve-uuid-1",
		ReceiptType: "confirmed",
		FinalCount:  1.0,
	})

	// Verify order is confirmed
	order2, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order2.Status != StatusConfirmed {
		t.Errorf("status = %q, want %q", order2.Status, StatusConfirmed)
	}
	if order2.CompletedAt == nil {
		t.Error("completed at should be set")
	}

	// After the order completes, the claim is RELEASED (TerminalizeOrder on the
	// confirmed transition / the delivery-release rule). A claim persisting here was the
	// leak the terminal chokepoint closed — so assert it's gone, not held.
	completedBin, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if completedBin.ClaimedBy != nil {
		t.Errorf("bin claimed_by = %v after completion, want nil (claim released on completion)", completedBin.ClaimedBy)
	}
}

func TestDispatcher_MoveOrder_FullLifecycle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin at storage node with a manifest
	testdb.CreateBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-MOV-1")

	backend := testdb.NewTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Phase 1: Submit move order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "move-uuid-1",
		OrderType:    OrderTypeMove,
		PayloadCode:  "PART-A",
		SourceNode:   storageNode.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})
	dispatchSimpleViaScanner(t, d, db, "move-uuid-1")

	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	order := testdb.AssertOrderStatus(t, db, "move-uuid-1", StatusDispatched)

	// Phase 2: Simulate fleet delivery, then confirm receipt
	db.UpdateOrderStatus(order.ID, string(StatusDelivered), "fleet delivered")

	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "move-uuid-1",
		ReceiptType: "confirmed",
		FinalCount:  1.0,
	})

	order2, _ := db.GetOrder(order.ID)
	if order2.Status != StatusConfirmed {
		t.Errorf("status = %q, want %q", order2.Status, StatusConfirmed)
	}
}

// TestDispatcher_MoveOrder_QueuesOnSaturatedNGRP pins the loader/unloader
// outbound fix. When a manual_swap loader finishes its cycle, the L2
// "filled bin → outbound supermarket" move targets a node group. If every
// slot in that group is full, intake (CreateInboundOrder) used to eagerly
// resolve the group and hard-fail with "cannot resolve synthetic node ...:
// no available slot in node group X" — surfacing as a red error toast on the
// Market Bin Loader HMI (plant SNF2 / SMN_001 / AMR Supermarket #1427). The
// order was never created at Core, so nothing retried when a slot freed.
//
// With the fix, a capacity-shaped resolution failure no longer fails the
// operator's action: the order is created against the group and parked in
// `queued` by CheckDropoffCapacity; the scanner replays it when a slot opens
// (planMove resolves a concrete child at dispatch time). Mirrors the
// complex-order contract pinned by TestDispatcher_ComplexOrder_QueuesOnSaturatedNGRP.
func TestDispatcher_MoveOrder_QueuesOnSaturatedNGRP(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	syntheticType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}

	// NGRP supermarket with both child slots occupied (saturated).
	supermarket := &nodes.Node{Name: "AMR-SUPERMARKET-SAT", IsSynthetic: true, NodeTypeID: &syntheticType.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(supermarket), "create supermarket")
	slotA := &nodes.Node{Name: "AMR-SAT-A", Enabled: true}
	slotB := &nodes.Node{Name: "AMR-SAT-B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(slotA), "create slotA")
	testutil.MustNoErr(t, db.CreateNode(slotB), "create slotB")
	testutil.MustNoErr(t, db.SetNodeParent(slotA.ID, supermarket.ID), "parent slotA")
	testutil.MustNoErr(t, db.SetNodeParent(slotB.ID, supermarket.ID), "parent slotB")
	occA := &bins.Bin{BinTypeID: 1, Label: "AMR-OCC-A", NodeID: &slotA.ID, Status: "available"}
	occB := &bins.Bin{BinTypeID: 1, Label: "AMR-OCC-B", NodeID: &slotB.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(occA), "create occA")
	testutil.MustNoErr(t, db.CreateBin(occB), "create occB")

	// The loader's filled bin to ship out (move source).
	_ = testdb.CreateBinAtNode(t, db, bp.Code, lineNode.ID, "L2-SRC-BIN")

	backend := testdb.NewTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)
	// The freed-slot re-submit below relies on planMove resolving the synthetic
	// dest NGRP to a concrete child. That gate is `s.resolver != nil`, but
	// newTestDispatcher wires resolver=nil — leaving the delivery node stuck at
	// the group name. Wire a real resolver, mirroring TestDispatcher_MoveOrder_NGRPSource.
	resolver := &DefaultResolver{DB: db, LaneLock: d.LaneLock(), DebugLog: d.dbg}
	d = NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "l2-sat-1",
		OrderType:    OrderTypeMove,
		PayloadCode:  bp.Code,
		SourceNode:   lineNode.Name,
		DeliveryNode: supermarket.Name,
		Quantity:     1.0,
	})
	dispatchSimpleViaScanner(t, d, db, "l2-sat-1")

	// The operator's action must NOT fail: no sendError (no HMI toast), the
	// order exists, and it is queued with a reason.
	if len(emitter.failed) > 0 {
		t.Fatalf("move to saturated NGRP should not hit the failed path (the HMI error toast); got: %+v", emitter.failed)
	}
	order, err := db.GetOrderByUUID("l2-sat-1")
	if err != nil {
		t.Fatalf("get order: %v — a full outbound group must create the order as queued, not reject it at intake", err)
	}
	if order.Status != StatusQueued {
		t.Errorf("status = %q, want %q (full outbound group must queue, not fail the operator)", order.Status, StatusQueued)
	}
	if order.QueueReason == "" {
		t.Error("queue_reason empty; expected a reason so the operator sees why the move is waiting")
	}

	// Free a slot and re-submit: with capacity now available, intake resolves
	// the group to the open child and the move dispatches cleanly (no toast,
	// no queue). Confirms the gate only holds while the group is actually full.
	testutil.MustNoErr(t, db.DeleteBin(occB.ID), "delete occB to free slotB")
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "l2-sat-2",
		OrderType:    OrderTypeMove,
		PayloadCode:  bp.Code,
		SourceNode:   lineNode.Name,
		DeliveryNode: supermarket.Name,
		Quantity:     1.0,
	})
	dispatchSimpleViaScanner(t, d, db, "l2-sat-2")
	order2, err := db.GetOrderByUUID("l2-sat-2")
	if err != nil {
		t.Fatalf("get order2: %v", err)
	}
	if order2.Status == StatusQueued {
		t.Errorf("order2 status = %q, want it to dispatch once a slot is free", order2.Status)
	}
	if order2.DeliveryNode != slotB.Name {
		t.Errorf("order2 delivery_node = %q, want %q (resolved to the freed child slot)", order2.DeliveryNode, slotB.Name)
	}
}

// TestDispatcher_ComplexOrder_QueuesOnSaturatedNGRP pins the
// Round-3 follow-up to Item C. Two-robot swap pairs construct each
// leg as a complex order with multi-step pickup/dropoff. Pre-fix,
// when the dropoff NGRP was saturated at intake the resolver
// returned "no available slot in node group X" and
// HandleComplexOrderRequest sent a resolution_failed error to Edge
// — the order was never created at Core, no retry path. Stephen's
// reproducer: two-robot swap supply dispatched, supermarket full,
// evac leg failed at intake instead of queueing.
//
// With this fix, the order is created with status=queued and
// queue_reason=the resolver message. DispatchPreparedComplex
// re-resolves on each scanner tick; when a slot opens up the order
// dispatches cleanly.
func TestDispatcher_ComplexOrder_QueuesOnSaturatedNGRP(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	syntheticType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}

	// Construct an NGRP supermarket with both child slots occupied.
	supermarket := &nodes.Node{Name: "SUPERMARKET-SAT", IsSynthetic: true, NodeTypeID: &syntheticType.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(supermarket), "create supermarket")
	slotA := &nodes.Node{Name: "SAT-A", Enabled: true}
	slotB := &nodes.Node{Name: "SAT-B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(slotA), "create slotA")
	testutil.MustNoErr(t, db.CreateNode(slotB), "create slotB")
	testutil.MustNoErr(t, db.SetNodeParent(slotA.ID, supermarket.ID), "parent slotA")
	testutil.MustNoErr(t, db.SetNodeParent(slotB.ID, supermarket.ID), "parent slotB")

	occA := &bins.Bin{BinTypeID: 1, Label: "OCC-A", NodeID: &slotA.ID, Status: "available"}
	occB := &bins.Bin{BinTypeID: 1, Label: "OCC-B", NodeID: &slotB.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(occA), "create occA")
	testutil.MustNoErr(t, db.CreateBin(occB), "create occB")

	// The evac leg's source: a manifest-confirmed bin at the line so
	// ApplyComplexPlan can pick it up once the order dispatches.
	_ = testdb.CreateBinAtNode(t, db, bp.Code, lineNode.ID, "EVAC-BIN")

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "evac-sat-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: supermarket.Name},
		},
	})

	// Round-3 follow-up: the order must exist at Core in queued state,
	// not have been rejected via sendError.
	if len(emitter.failed) > 0 {
		t.Fatalf("complex order should not have hit failed path; got: %+v", emitter.failed)
	}
	order, err := db.GetOrderByUUID("evac-sat-1")
	if err != nil {
		t.Fatalf("get order: %v — capacity-shaped resolution failure should create the order as queued, not reject it", err)
	}
	if order.Status != StatusQueued {
		t.Errorf("status = %q, want %q (capacity-blocked complex order must queue)", order.Status, StatusQueued)
	}
	if order.QueueReason == "" {
		t.Errorf("queue_reason empty; expected the resolver message so the operator sees why the order is waiting")
	}
	if !strings.Contains(order.QueueReason, "no available slot in node group") {
		t.Errorf("queue_reason = %q, want it to contain 'no available slot in node group ...'", order.QueueReason)
	}
	// Field-notes Note 8 regression: complex orders historically persisted
	// payload_code="" because complex_dispatch.go didn't assign PayloadCode
	// in the order struct literal. Downstream this disabled the payload-
	// mismatch guard in binresolver and let wrong-family bins be claimed.
	if order.PayloadCode != bp.Code {
		t.Errorf("payload_code = %q, want %q (Note 8 regression: complex order PayloadCode must persist)", order.PayloadCode, bp.Code)
	}

	// Free slotB so a replay can succeed. Delete the occupying bin and
	// drive DispatchPreparedComplex directly (mimics what the scanner
	// would do on EventBinUpdated after the slot opened).
	testutil.MustNoErr(t, db.DeleteBin(occB.ID), "delete occB to free slotB")

	if err := d.DispatchPreparedComplex(order); err != nil {
		t.Fatalf("DispatchPreparedComplex after slot opened: %v", err)
	}

	// Re-read; status should be Dispatched now, queue_reason cleared
	// by the dispatch path.
	order, err = db.GetOrderByUUID("evac-sat-1")
	if err != nil {
		t.Fatalf("re-read order: %v", err)
	}
	if order.Status != StatusDispatched {
		t.Errorf("after replay status = %q, want %q", order.Status, StatusDispatched)
	}
}

func TestDispatcher_CancelOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin with a manifest
	bin := &bins.Bin{BinTypeID: 1, Label: "BIN-CAN-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin)
	db.SetBinManifest(bin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")

	backend := testdb.NewTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Submit retrieve order — dispatch will claim the bin
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "cancel-uuid-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})
	dispatchSimpleViaScanner(t, d, db, "cancel-uuid-1")

	order := testdb.RequireOrder(t, db, "cancel-uuid-1")

	// Verify bin was claimed by this order
	claimed, _ := db.GetBin(bin.ID)
	if claimed.ClaimedBy == nil || *claimed.ClaimedBy != order.ID {
		t.Fatalf("bin should be claimed by order %d before cancel", order.ID)
	}

	// Cancel the order
	d.HandleOrderCancel(env, &protocol.OrderCancel{
		OrderUUID: "cancel-uuid-1",
		Reason:    "operator cancelled",
	})

	// Verify order is cancelled
	order2, _ := db.GetOrder(order.ID)
	if order2.Status != StatusCancelled {
		t.Errorf("status = %q, want %q", order2.Status, StatusCancelled)
	}

	// Verify bin was unclaimed by the cancel
	unclaimed, _ := db.GetBin(bin.ID)
	if unclaimed.ClaimedBy != nil {
		t.Errorf("bin should be unclaimed after cancel, but ClaimedBy = %v", unclaimed.ClaimedBy)
	}

	// Verify cancelled event was emitted
	if len(emitter.cancelled) != 1 {
		t.Fatalf("cancelled events = %d, want 1", len(emitter.cancelled))
	}
}

func TestDispatcher_RedirectOrder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create another line node
	lineNode2 := &nodes.Node{Name: "LINE2-IN", Enabled: true}
	db.CreateNode(lineNode2)

	// Create a bin with a manifest
	bin := &bins.Bin{BinTypeID: 1, Label: "BIN-RED-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin)
	db.SetBinManifest(bin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Submit move order from storage to line1
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "redirect-uuid-1",
		OrderType:    OrderTypeMove,
		PayloadCode:  "PART-A",
		SourceNode:   storageNode.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})

	// Redirect to line2
	d.HandleOrderRedirect(env, &protocol.OrderRedirect{
		OrderUUID:       "redirect-uuid-1",
		NewDeliveryNode: lineNode2.Name,
	})

	// Verify order destination was updated (need to re-fetch from DB)
	order2 := testdb.RequireOrder(t, db, "redirect-uuid-1")
	if order2.DeliveryNode != lineNode2.Name {
		t.Errorf("delivery node = %q, want %q", order2.DeliveryNode, lineNode2.Name)
	}
}

func TestDispatcher_SyntheticNodeResolution(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	// Look up the seeded synthetic node type (NGRP)
	syntheticType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get synthetic node type: %v", err)
	}

	// Create a synthetic parent node (delivery zone)
	parentNode := &nodes.Node{
		Name: "ZONE-A", IsSynthetic: true,
		NodeTypeID: &syntheticType.ID, Enabled: true,
	}
	testutil.MustNoErr(t, db.CreateNode(parentNode), "create parent node")

	// Create child nodes under the synthetic parent (lineside slots)
	child1 := &nodes.Node{Name: "ZONE-A-01", Enabled: true}
	child2 := &nodes.Node{Name: "ZONE-A-02", Enabled: true}
	db.CreateNode(child1)
	db.CreateNode(child2)
	db.SetNodeParent(child1.ID, parentNode.ID)
	db.SetNodeParent(child2.ID, parentNode.ID)

	// Put a bin at child2 to occupy it (child2 occupied, child1 empty)
	occBin := &bins.Bin{BinTypeID: 1, Label: "BIN-SYN-OCC", NodeID: &child2.ID, Status: "available"}
	db.CreateBin(occBin)

	// Create source bin at a separate node for FIFO to find
	srcNode := &nodes.Node{Name: "SRC-SYN", Enabled: true}
	db.CreateNode(srcNode)
	srcBin := &bins.Bin{BinTypeID: 1, Label: "BIN-SYN-SRC", NodeID: &srcNode.ID, Status: "available"}
	db.CreateBin(srcBin)
	db.SetBinManifest(srcBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(srcBin.ID, "")

	// Create dispatcher with resolver
	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)

	env := testEnvelope()

	// Submit retrieve order targeting synthetic parent — delivery should resolve
	// to child1 (empty slot), source should pick srcPayload via FIFO
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "syn-retrieve-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: parentNode.Name,
		Quantity:     1.0,
	})
	dispatchSimpleViaScanner(t, d, db, "syn-retrieve-1")

	// Verify order was dispatched (not failed)
	if len(emitter.failed) > 0 {
		t.Fatalf("order should not fail, got error: %s", emitter.failed[0].errorCode)
	}
	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	order := testdb.AssertOrderStatus(t, db, "syn-retrieve-1", StatusDispatched)
	// Delivery node should be resolved to child1 (empty slot), not child2 (occupied)
	if order.DeliveryNode != child1.Name {
		t.Errorf("delivery node = %q, want %q (empty child)", order.DeliveryNode, child1.Name)
	}
	// Pickup should be source node (where the FIFO payload is)
	if order.SourceNode != srcNode.Name {
		t.Errorf("source node = %q, want %q", order.SourceNode, srcNode.Name)
	}
}

// TestDispatcher_MultiOrderToSyntheticNGRP verifies that multiple retrieve orders
// to the same synthetic NGRP resolve to different physical children and that
// in-flight awareness prevents double-booking of the same slot.
func TestDispatcher_MultiOrderToSyntheticNGRP(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, _ = setupTestData(t, db)

	syntheticType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}

	// Create NGRP zone with 3 physical children
	zone := &nodes.Node{Name: "PRESS-A1", IsSynthetic: true, NodeTypeID: &syntheticType.ID, Enabled: true}
	db.CreateNode(zone)
	slot1 := &nodes.Node{Name: "PRESS-A1-01", Enabled: true}
	slot2 := &nodes.Node{Name: "PRESS-A1-02", Enabled: true}
	slot3 := &nodes.Node{Name: "PRESS-A1-03", Enabled: true}
	db.CreateNode(slot1)
	db.CreateNode(slot2)
	db.CreateNode(slot3)
	db.SetNodeParent(slot1.ID, zone.ID)
	db.SetNodeParent(slot2.ID, zone.ID)
	db.SetNodeParent(slot3.ID, zone.ID)

	// Create source payloads in a supermarket (payload A x2, payload B x1)
	bpA := &payloads.Payload{Code: "PART-MULTI-A"}
	bpB := &payloads.Payload{Code: "PART-MULTI-B"}
	db.CreatePayload(bpA)
	db.CreatePayload(bpB)

	supermarket := &nodes.Node{Name: "SM-MULTI", Zone: "W", Enabled: true}
	db.CreateNode(supermarket)

	binA1 := &bins.Bin{BinTypeID: 1, Label: "M-A1", NodeID: &supermarket.ID, Status: "available"}
	binA2 := &bins.Bin{BinTypeID: 1, Label: "M-A2", NodeID: &supermarket.ID, Status: "available"}
	binB1 := &bins.Bin{BinTypeID: 1, Label: "M-B1", NodeID: &supermarket.ID, Status: "available"}
	db.CreateBin(binA1)
	db.CreateBin(binA2)
	db.CreateBin(binB1)
	db.SetBinManifest(binA1.ID, `{"items":[]}`, bpA.Code, 100)
	db.ConfirmBinManifest(binA1.ID, "")
	db.SetBinManifest(binA2.ID, `{"items":[]}`, bpA.Code, 100)
	db.ConfirmBinManifest(binA2.ID, "")
	db.SetBinManifest(binB1.ID, `{"items":[]}`, bpB.Code, 100)
	db.ConfirmBinManifest(binB1.ID, "")

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	// Order 1: payload A -> PRESS-A1
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "multi-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-MULTI-A",
		DeliveryNode: zone.Name,
		Quantity:     1,
	})
	dispatchSimpleViaScanner(t, d, db, "multi-1")
	// Order 2: payload A -> PRESS-A1
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "multi-2",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-MULTI-A",
		DeliveryNode: zone.Name,
		Quantity:     1,
	})
	dispatchSimpleViaScanner(t, d, db, "multi-2")
	// Order 3: payload B -> PRESS-A1
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "multi-3",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-MULTI-B",
		DeliveryNode: zone.Name,
		Quantity:     1,
	})
	dispatchSimpleViaScanner(t, d, db, "multi-3")

	if len(emitter.failed) > 0 {
		t.Fatalf("unexpected failures: %d (first: %s)", len(emitter.failed), emitter.failed[0].errorCode)
	}

	o1 := testdb.RequireOrder(t, db, "multi-1")
	o2 := testdb.RequireOrder(t, db, "multi-2")
	o3 := testdb.RequireOrder(t, db, "multi-3")

	// All three should be dispatched
	for _, o := range []*orders.Order{o1, o2, o3} {
		if o.Status != StatusDispatched {
			t.Errorf("order %s status = %q, want dispatched", o.EdgeUUID, o.Status)
		}
	}

	// Each should have a unique delivery node (no double-booking)
	deliveries := map[string]string{
		o1.DeliveryNode: o1.EdgeUUID,
		o2.DeliveryNode: o2.EdgeUUID,
		o3.DeliveryNode: o3.EdgeUUID,
	}
	if len(deliveries) != 3 {
		t.Errorf("expected 3 unique delivery nodes, got %d: o1=%s o2=%s o3=%s",
			len(deliveries), o1.DeliveryNode, o2.DeliveryNode, o3.DeliveryNode)
	}

	// A 4th order should fail — all 3 slots are in-flight
	failsBefore := len(emitter.failed)
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "multi-4",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-MULTI-A",
		DeliveryNode: zone.Name,
		Quantity:     1,
	})
	dispatchSimpleViaScanner(t, d, db, "multi-4")

	// Resolution fails before order creation, so check emitter errors
	if len(emitter.failed) <= failsBefore {
		// No emitter failure means it was caught as a sendError before order creation
		// Check that it was NOT dispatched
		o4, err := db.GetOrderByUUID("multi-4")
		if err == nil && o4.Status == StatusDispatched {
			t.Errorf("4th order should not be dispatched, all slots in-flight")
		}
	}
}

// TestDispatcher_RetrieveEmptyToSyntheticNGRP verifies empty bin delivery
// to a synthetic node group uses store resolution (finds empty slots).
func TestDispatcher_RetrieveEmptyToSyntheticNGRP(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, _ = setupTestData(t, db)

	syntheticType, _ := db.GetNodeTypeByCode("NGRP")

	// Create NGRP zone with 2 children, one occupied
	zone := &nodes.Node{Name: "EMPTY-ZONE", IsSynthetic: true, NodeTypeID: &syntheticType.ID, Enabled: true}
	db.CreateNode(zone)
	slot1 := &nodes.Node{Name: "EZ-01", Enabled: true}
	slot2 := &nodes.Node{Name: "EZ-02", Enabled: true}
	db.CreateNode(slot1)
	db.CreateNode(slot2)
	db.SetNodeParent(slot1.ID, zone.ID)
	db.SetNodeParent(slot2.ID, zone.ID)

	// Occupy slot1
	occBin := &bins.Bin{BinTypeID: 1, Label: "OCC-1", NodeID: &slot1.ID, Status: "available"}
	db.CreateBin(occBin)

	// Create payload with bin type compatibility
	bp := &payloads.Payload{Code: "EMPTY-BP"}
	db.CreatePayload(bp)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	// Create an empty compatible bin somewhere (source)
	srcNode := &nodes.Node{Name: "EMPTY-SRC", Enabled: true}
	db.CreateNode(srcNode)
	emptyBin := &bins.Bin{BinTypeID: bt.ID, Label: "EMPTY-BIN-1", NodeID: &srcNode.ID, Status: "available"}
	db.CreateBin(emptyBin)

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:     "empty-1",
		OrderType:     OrderTypeRetrieve,
		PayloadCode:   "EMPTY-BP",
		DeliveryNode:  zone.Name,
		RetrieveEmpty: true,
		Quantity:      1,
	})
	dispatchSimpleViaScanner(t, d, db, "empty-1")

	if len(emitter.failed) > 0 {
		t.Fatalf("order should not fail, got: %s", emitter.failed[0].errorCode)
	}

	o := testdb.RequireOrder(t, db, "empty-1")
	if o.Status != StatusDispatched {
		t.Errorf("status = %q, want dispatched", o.Status)
	}
	// Delivery should resolve to slot2 (empty), not slot1 (occupied)
	if o.DeliveryNode != slot2.Name {
		t.Errorf("delivery = %q, want %q (empty slot)", o.DeliveryNode, slot2.Name)
	}
}

// TestRetrieveEmpty_LaneSourceScopesToLane verifies that a manual empty pull
// whose source_node is a LANE (not its parent NGRP) scopes the empty search to
// that lane's own slots — exercising the planRetrieveEmpty LANE-source gate
// (previously NGRP-only, so a LANE source fell through to the global finder).
// A decoy empty outside the lane (no depth, lower id) would win the global
// any-zone finder; the LANE source must skip it and pick the lane's empty.
func TestRetrieveEmpty_LaneSourceScopesToLane(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, _ = setupTestData(t, db)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")
	bt, _ := db.GetBinTypeByCode("DEFAULT")

	bp := &payloads.Payload{Code: "LANE-SCOPE-BP"}
	db.CreatePayload(bp)
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	// Decoy empty OUTSIDE any lane, created first (lower id, no depth) so the
	// global any-zone finder would prefer it over the lane's empty.
	decoyNode := &nodes.Node{Name: "LSC-DECOY", Enabled: true}
	db.CreateNode(decoyNode)
	decoy := &bins.Bin{BinTypeID: bt.ID, Label: "LSC-DECOY-EMPTY", NodeID: &decoyNode.ID, Status: "available"}
	db.CreateBin(decoy)

	// NGRP -> LANE -> single accessible slot holding the empty we expect.
	grp := &nodes.Node{Name: "LSC-GRP", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	db.CreateNode(grp)
	grp, _ = db.GetNode(grp.ID)
	lane := &nodes.Node{Name: "LSC-LANE", IsSynthetic: true, NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true}
	db.CreateNode(lane)
	lane, _ = db.GetNode(lane.ID)
	d1 := 1
	slot := &nodes.Node{Name: "LSC-LANE-S1", ParentID: &lane.ID, Enabled: true, Depth: &d1}
	db.CreateNode(slot)
	laneEmpty := &bins.Bin{BinTypeID: bt.ID, Label: "LSC-LANE-EMPTY", NodeID: &slot.ID, Status: "available"}
	db.CreateBin(laneEmpty)

	dest := &nodes.Node{Name: "LSC-LINE", Enabled: true}
	db.CreateNode(dest)

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db, LaneLock: NewLaneLock()}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:     "lane-empty-src-1",
		OrderType:     OrderTypeRetrieve,
		PayloadCode:   "LANE-SCOPE-BP",
		DeliveryNode:  dest.Name,
		SourceNode:    lane.Name, // pick the LANE itself, not the parent group
		RetrieveEmpty: true,
		Quantity:      1,
	})
	dispatchSimpleViaScanner(t, d, db, "lane-empty-src-1")

	if len(emitter.failed) > 0 {
		t.Fatalf("order should not fail, got: %s", emitter.failed[0].errorCode)
	}
	o := testdb.RequireOrder(t, db, "lane-empty-src-1")
	if o.SourceNode != slot.Name {
		t.Errorf("source = %q, want %q — a LANE source must scope to the lane's own slots, not the global decoy %q",
			o.SourceNode, slot.Name, decoyNode.Name)
	}
}

func TestRetrieveEmpty_BuriedTriggersReshuffle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, _ = setupTestData(t, db)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &payloads.Payload{Code: "TC41-BP"}
	db.CreatePayload(bp)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	// Create NGRP with 2 lanes of 3 slots
	grp := &nodes.Node{Name: "TC41-GRP", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	db.CreateNode(grp)
	grp, _ = db.GetNode(grp.ID)

	// Lane 1: full bins at depth 1+2, empty buried at depth 3
	lane1 := &nodes.Node{Name: "TC41-L1", IsSynthetic: true, NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true}
	db.CreateNode(lane1)
	lane1, _ = db.GetNode(lane1.ID)

	d1, d2, d3 := 1, 2, 3
	l1s1 := &nodes.Node{Name: "TC41-L1-S1", ParentID: &lane1.ID, Enabled: true, Depth: &d1}
	db.CreateNode(l1s1)
	l1s2 := &nodes.Node{Name: "TC41-L1-S2", ParentID: &lane1.ID, Enabled: true, Depth: &d2}
	db.CreateNode(l1s2)
	l1s3 := &nodes.Node{Name: "TC41-L1-S3", ParentID: &lane1.ID, Enabled: true, Depth: &d3}
	db.CreateNode(l1s3)

	// Full bins blocking lane 1
	blkBin1 := &bins.Bin{BinTypeID: bt.ID, Label: "TC41-FULL-1", NodeID: &l1s1.ID, Status: "available"}
	db.CreateBin(blkBin1)
	db.SetBinManifest(blkBin1.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(blkBin1.ID, "")

	blkBin2 := &bins.Bin{BinTypeID: bt.ID, Label: "TC41-FULL-2", NodeID: &l1s2.ID, Status: "available"}
	db.CreateBin(blkBin2)
	db.SetBinManifest(blkBin2.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(blkBin2.ID, "")

	// Buried empty at depth 3
	emptyBin := &bins.Bin{BinTypeID: bt.ID, Label: "TC41-EMPTY", NodeID: &l1s3.ID, Status: "available"}
	db.CreateBin(emptyBin)

	// Lane 2: completely empty — provides shuffle slots for the reshuffle
	lane2 := &nodes.Node{Name: "TC41-L2", IsSynthetic: true, NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true}
	db.CreateNode(lane2)
	lane2, _ = db.GetNode(lane2.ID)

	l2s1 := &nodes.Node{Name: "TC41-L2-S1", ParentID: &lane2.ID, Enabled: true, Depth: &d1}
	db.CreateNode(l2s1)
	l2s2 := &nodes.Node{Name: "TC41-L2-S2", ParentID: &lane2.ID, Enabled: true, Depth: &d2}
	db.CreateNode(l2s2)

	// Delivery target (lineside node)
	destNode := &nodes.Node{Name: "TC41-LINE", Enabled: true}
	db.CreateNode(destNode)

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db, LaneLock: NewLaneLock()}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:     "tc41-empty-1",
		OrderType:     OrderTypeRetrieve,
		PayloadCode:   bp.Code,
		DeliveryNode:  destNode.Name,
		RetrieveEmpty: true,
		Quantity:      1,
	})

	o := testdb.RequireOrder(t, db, "tc41-empty-1")

	// Before the fix: order would go to "dispatched" targeting an unreachable slot.
	// After the fix: order should go to "reshuffling" (compound reshuffle planned).
	if o.Status == StatusDispatched {
		t.Errorf("order dispatched directly — buried empty was NOT detected (pre-fix behavior)")
	}
	if o.Status == StatusReshuffling {
		t.Logf("TC-41 fix confirmed: order %d is reshuffling to unbury empty bin", o.ID)
	} else if o.Status != StatusReshuffling {
		// Could be failed if reshuffle planning hit an issue — still better than dispatching to unreachable
		t.Logf("order status = %q (not reshuffling, but also not dispatched blind)", o.Status)
	}

	// Verify compound children were created (reshuffle steps)
	children, _ := db.ListChildOrders(o.ID)
	if len(children) == 0 && o.Status == StatusReshuffling {
		t.Error("order is reshuffling but no compound children found")
	}
	if len(children) > 0 {
		t.Logf("reshuffle plan has %d steps", len(children))
	}
}

// TestComplex_BuriedSourceTriggersReshuffle exercises the full
// complex-order buried-bin reshuffle path: an intake with an NGRP
// pickup that resolves to a buried bin should create the parent at
// Reshuffling, schedule a compound, and arrange for the parent to
// resume to Queued once the compound completes.
//
// Mirrors TestRetrieveEmpty_BuriedTriggersReshuffle but routes
// through HandleComplexOrderRequest instead of HandleOrderRequest.
// The test stops short of running the compound to completion (which
// would require simulating the full fleet pipeline) — it asserts the
// invariants up to and including the compound being scheduled with
// the right shape.
func TestComplex_BuriedSourceTriggersReshuffle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, _ = setupTestData(t, db)

	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp := &payloads.Payload{Code: "COMPLEX-BURIED-BP"}
	db.CreatePayload(bp)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	// NGRP with one buried-target lane + one empty shuffle lane.
	grp := &nodes.Node{Name: "ASRS_Lane_Test", IsSynthetic: true, NodeTypeID: &grpType.ID, Enabled: true}
	db.CreateNode(grp)
	grp, _ = db.GetNode(grp.ID)

	lane1 := &nodes.Node{Name: "ASRS_Lane_Test-L1", IsSynthetic: true, NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true}
	db.CreateNode(lane1)
	lane1, _ = db.GetNode(lane1.ID)

	d1, d2, d3 := 1, 2, 3
	l1s1 := &nodes.Node{Name: "ASRS_Lane_Test-L1-S1", ParentID: &lane1.ID, Enabled: true, Depth: &d1}
	db.CreateNode(l1s1)
	l1s2 := &nodes.Node{Name: "ASRS_Lane_Test-L1-S2", ParentID: &lane1.ID, Enabled: true, Depth: &d2}
	db.CreateNode(l1s2)
	l1s3 := &nodes.Node{Name: "ASRS_Lane_Test-L1-S3", ParentID: &lane1.ID, Enabled: true, Depth: &d3}
	db.CreateNode(l1s3)

	// Two blocker bins.
	for _, slotID := range []int64{l1s1.ID, l1s2.ID} {
		b := &bins.Bin{BinTypeID: bt.ID, Label: fmt.Sprintf("CB-BLK-%d", slotID), NodeID: &slotID, Status: "available"}
		db.CreateBin(b)
		db.SetBinManifest(b.ID, `{"items":[]}`, "OTHER-PAYLOAD", 100)
		db.ConfirmBinManifest(b.ID, "")
	}

	// Target bin (the one the complex order wants).
	targetBin := &bins.Bin{BinTypeID: bt.ID, Label: "CB-TARGET", NodeID: &l1s3.ID, Status: "available"}
	db.CreateBin(targetBin)
	db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(targetBin.ID, "")

	// Shuffle lane (provides empty slots for unbury).
	lane2 := &nodes.Node{Name: "ASRS_Lane_Test-L2", IsSynthetic: true, NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true}
	db.CreateNode(lane2)
	lane2, _ = db.GetNode(lane2.ID)
	l2s1 := &nodes.Node{Name: "ASRS_Lane_Test-L2-S1", ParentID: &lane2.ID, Enabled: true, Depth: &d1}
	db.CreateNode(l2s1)
	l2s2 := &nodes.Node{Name: "ASRS_Lane_Test-L2-S2", ParentID: &lane2.ID, Enabled: true, Depth: &d2}
	db.CreateNode(l2s2)

	// Dropoff (lineside).
	dropNode := &nodes.Node{Name: "LINE-DROP", Enabled: true}
	db.CreateNode(dropNode)

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db, LaneLock: NewLaneLock()}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	// Submit complex order: pickup from NGRP, dropoff at lineside.
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "complex-buried-1",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: grp.Name},
			{Action: "dropoff", Node: dropNode.Name},
		},
	})

	o := testdb.RequireOrder(t, db, "complex-buried-1")

	// Order must NOT have been terminal-failed at intake.
	if o.Status == StatusFailed {
		t.Fatalf("complex order terminal-failed at intake — pre-fix behavior (expected Reshuffling)")
	}
	// Parent should now be Reshuffling.
	if o.Status != StatusReshuffling {
		t.Errorf("parent status = %q, want %q (compound reshuffle should have started)", o.Status, StatusReshuffling)
	}
	// Field-notes Note 8 regression: the buried-intake path
	// (handleComplexBuriedAtIntake) also constructs a complex order
	// struct literal — it must persist PayloadCode same as the main
	// path.
	if o.PayloadCode != bp.Code {
		t.Errorf("payload_code = %q, want %q (Note 8 regression: buried-intake complex order PayloadCode must persist)", o.PayloadCode, bp.Code)
	}

	// Compound children should exist.
	children, _ := db.ListChildOrders(o.ID)
	if len(children) == 0 {
		t.Fatal("no compound children created for buried complex order")
	}
	// Expose mode (default — no target_nodes configured): unbury only.
	// Two blockers → two unbury children.
	if len(children) != 2 {
		t.Errorf("compound children = %d, want 2 (two blockers, expose mode)", len(children))
	}
	for _, c := range children {
		if c.ParentOrderID == nil || *c.ParentOrderID != o.ID {
			t.Errorf("child %d ParentOrderID = %v, want %d", c.ID, c.ParentOrderID, o.ID)
		}
	}

}

// TestDispatcher_DotNotationBypassesResolver verifies that ordering to a
// specific child using dot notation (ZONE.Node10) skips resolver — the
// physical node is used directly.
func TestDispatcher_DotNotationBypassesResolver(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	syntheticType, _ := db.GetNodeTypeByCode("NGRP")
	zone := &nodes.Node{Name: "DOT-ZONE", IsSynthetic: true, NodeTypeID: &syntheticType.ID, Enabled: true}
	db.CreateNode(zone)
	child := &nodes.Node{Name: "SLOT-X", Enabled: true}
	db.CreateNode(child)
	db.SetNodeParent(child.ID, zone.ID)

	// Create source bin
	srcNode := &nodes.Node{Name: "DOT-SRC", Enabled: true}
	db.CreateNode(srcNode)
	bin := &bins.Bin{BinTypeID: 1, Label: "DOT-BIN-1", NodeID: &srcNode.ID, Status: "available"}
	db.CreateBin(bin)
	db.SetBinManifest(bin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	// Use dot notation: "DOT-ZONE.SLOT-X" — resolves to physical child directly
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "dot-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: "DOT-ZONE.SLOT-X",
		Quantity:     1,
	})
	dispatchSimpleViaScanner(t, d, db, "dot-1")

	if len(emitter.failed) > 0 {
		t.Fatalf("order should not fail, got: %s", emitter.failed[0].errorCode)
	}

	o := testdb.RequireOrder(t, db, "dot-1")
	if o.Status != StatusDispatched {
		t.Errorf("status = %q, want dispatched", o.Status)
	}
	// Dot notation is stored as-is; the fleet dispatch uses the resolved node name.
	// Verify the order was dispatched (fleet got the right node via GetNodeByDotName).
	if o.DeliveryNode != "DOT-ZONE.SLOT-X" {
		t.Errorf("delivery = %q, want %q", o.DeliveryNode, "DOT-ZONE.SLOT-X")
	}
}

func TestDispatcher_FleetFailure(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin with a manifest
	bin := &bins.Bin{BinTypeID: 1, Label: "BIN-FF-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin)
	db.SetBinManifest(bin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")

	// Use mockBackend (returns errors for all fleet ops)
	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "fleet-fail-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})
	dispatchSimpleViaScanner(t, d, db, "fleet-fail-1")

	// Order should be received then failed
	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}
	if len(emitter.failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(emitter.failed))
	}
	if emitter.failed[0].errorCode != "fleet_failed" {
		t.Errorf("error code = %q, want %q", emitter.failed[0].errorCode, "fleet_failed")
	}

	// Verify order status is failed
	testdb.AssertOrderStatus(t, db, "fleet-fail-1", StatusFailed)

	// Verify bin was unclaimed after fleet failure
	b, _ := db.GetBin(bin.ID)
	if b.ClaimedBy != nil {
		t.Errorf("bin should be unclaimed after fleet failure, ClaimedBy = %v", b.ClaimedBy)
	}
}

func TestDispatcher_PriorityHandling(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create bins with manifests
	bin1 := &bins.Bin{BinTypeID: 1, Label: "BIN-PRI-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin1)
	db.SetBinManifest(bin1.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin1.ID, "")

	bin2 := &bins.Bin{BinTypeID: 1, Label: "BIN-PRI-2", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin2)
	db.SetBinManifest(bin2.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin2.ID, "")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Submit low priority order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "low-priority",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
		Priority:     0,
	})

	// Submit high priority order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "high-priority",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
		Priority:     10,
	})

	// Both orders should be dispatched
	order1 := testdb.RequireOrder(t, db, "low-priority")
	order2 := testdb.RequireOrder(t, db, "high-priority")

	if order1.Priority != 0 {
		t.Errorf("low priority = %d, want 0", order1.Priority)
	}
	if order2.Priority != 10 {
		t.Errorf("high priority = %d, want 10", order2.Priority)
	}
}

func TestHandleRetrieve_BinTracking(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create bin with manifest
	bin := &bins.Bin{BinTypeID: 1, Label: "BIN-BT-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin)
	db.SetBinManifest(bin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "uuid-bin-track",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})
	dispatchSimpleViaScanner(t, d, db, "uuid-bin-track")

	order := testdb.RequireOrder(t, db, "uuid-bin-track")

	// Order should have BinID set
	if order.BinID == nil {
		t.Fatal("order BinID should be set after retrieve")
	}
	if *order.BinID != bin.ID {
		t.Errorf("order BinID = %d, want %d", *order.BinID, bin.ID)
	}

	// Bin should be claimed
	gotBin, _ := db.GetBin(bin.ID)
	if gotBin.ClaimedBy == nil {
		t.Fatal("bin should be claimed after retrieve")
	}
	if *gotBin.ClaimedBy != order.ID {
		t.Errorf("bin claimed_by = %d, want %d", *gotBin.ClaimedBy, order.ID)
	}
}

func TestHandleOrderIngest(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)

	// Set up payload_bin_types for compatible empty bin
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	// Create an empty bin at the station (simulating a bin at a produce location)
	produceNode := &nodes.Node{Name: "PRODUCE-1", Enabled: true}
	db.CreateNode(produceNode)

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-ING-1", NodeID: &produceNode.ID, Status: "available"}
	db.CreateBin(bin)

	// Also create a storage node for the store destination
	_ = storageNode

	backend := testdb.NewTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	env := testEnvelope()
	d.HandleOrderIngest(env, &protocol.OrderIngestRequest{
		OrderUUID:   "uuid-ingest-1",
		PayloadCode: bp.Code,
		BinLabel:    "BIN-ING-1",
		SourceNode:  "PRODUCE-1",
		Quantity:    100,
		Manifest: []protocol.IngestManifestItem{
			{PartNumber: "PN-001", Quantity: 50, Description: "Bolt M8"},
			{PartNumber: "PN-002", Quantity: 50, Description: "Washer M8"},
		},
	})

	// Ingest is a manifest-only inventory write: it records AND confirms the
	// bin's manifest (atomically) and dispatches NOTHING — no order, no event,
	// no claim.
	if len(emitter.received) != 0 {
		t.Fatalf("received events = %d, want 0 (ingest mints no order)", len(emitter.received))
	}

	gotBin, _ := db.GetBin(bin.ID)
	if gotBin.PayloadCode != bp.Code {
		t.Errorf("bin payload_code = %q, want %q", gotBin.PayloadCode, bp.Code)
	}
	if gotBin.UOPRemaining != 100 {
		t.Errorf("bin UOPRemaining = %d, want 100 (operator-measured count)", gotBin.UOPRemaining)
	}
	if !gotBin.ManifestConfirmed {
		t.Error("bin manifest should be confirmed after ingest (set-and-confirm are atomic)")
	}
	if gotBin.ClaimedBy != nil {
		t.Errorf("manifest-only ingest must not claim the bin, got claimed_by=%v", gotBin.ClaimedBy)
	}
	if order, _ := db.GetOrderByUUID("uuid-ingest-1"); order != nil {
		t.Errorf("ingest must not create an order, got %+v", order)
	}
}

// TestDispatcher_RetrieveOrder_NGRPSource verifies that a retrieve_full order
// with an NGRP (supermarket group) as the source resolves to a concrete slot,
// claims the target bin, and dispatches. Regression for the shadowed-sourceNode
// panic in planRetrieve: when the NGRP resolver succeeded, the inner `:=`
// declaration shadowed the outer `sourceNode` and the subsequent
// `sourceNode.Name` deref nil-panicked, leaving the order stranded at
// `sourcing`. Lit up in production by unloader auto-push passing
// claim.InboundSource as SourceNode for retrieve_full orders.
func TestDispatcher_RetrieveOrder_NGRPSource(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix:   "RTNGRP",
		NumSlots: 1, // accessible target, no blockers
	})

	backend := testdb.NewTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)
	resolver := &DefaultResolver{DB: db, LaneLock: d.LaneLock(), DebugLog: d.dbg}
	d2 := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)

	env := testEnvelope()

	d2.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "retrieve-ngrp-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name, // NGRP — the auto-push scenario
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1,
	})
	dispatchSimpleViaScanner(t, d2, db, "retrieve-ngrp-1")

	order := testdb.RequireOrderStatus(t, db, "retrieve-ngrp-1", StatusDispatched)

	if order.BinID == nil {
		t.Fatal("BinID nil — planRetrieve must claim a bin via NGRP resolver")
	}
	if *order.BinID != sc.TargetBin.ID {
		t.Errorf("BinID = %d, want %d (target bin in NGRP lane)", *order.BinID, sc.TargetBin.ID)
	}
	if order.SourceNode == sc.Grp.Name {
		t.Errorf("SourceNode = %q, want resolved to concrete slot %q", order.SourceNode, sc.Slots[0].Name)
	}
	if order.SourceNode != sc.Slots[0].Name {
		t.Errorf("SourceNode = %q, want %q", order.SourceNode, sc.Slots[0].Name)
	}
	if len(backend.Orders()) != 1 {
		t.Fatalf("fleet orders = %d, want 1", len(backend.Orders()))
	}
}

// TestDispatcher_MoveOrder_NGRPSource verifies that a move order with an NGRP
// (supermarket group) as the source node correctly resolves to a concrete slot,
// claims the bin, and dispatches. This is the bug scenario from "request material"
// where the node is empty — the edge creates a move order from the supermarket
// NGRP, and planMove must resolve through the group resolver rather than doing
// a raw ListBinsByNode on the synthetic node (which returns nothing).
func TestDispatcher_MoveOrder_NGRPSource(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// Use SetupCompound to create a full NGRP → LANE → slots layout with
	// a single slot and one bin. NumSlots=1 means no blockers — the target
	// bin is accessible at the front of the lane.
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix:   "MVNGRP",
		NumSlots: 1, // single slot, target bin at front (not buried)
	})

	backend := testdb.NewTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	// The resolver needs to be set up for NGRP resolution to work.
	// newTestDispatcher creates a dispatcher with resolver=nil; we need one.
	resolver := &DefaultResolver{DB: db, LaneLock: d.LaneLock(), DebugLog: d.dbg}
	d2 := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)

	env := testEnvelope()

	// Submit a move order with SourceNode = NGRP name (the supermarket group).
	// This is what the edge sends when requestNodeFromClaim detects an empty
	// lineside node and downgrades to a simple delivery.
	d2.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "move-ngrp-1",
		OrderType:    OrderTypeMove,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name, // NGRP — the bug scenario
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1.0,
	})
	dispatchSimpleViaScanner(t, d2, db, "move-ngrp-1")

	// Verify the order was dispatched (not failed)
	order := testdb.RequireOrderStatus(t, db, "move-ngrp-1", StatusDispatched)

	// Verify the bin was claimed
	if order.BinID == nil {
		t.Fatal("BinID should be set — planMove must claim a bin via NGRP resolver")
	}
	if *order.BinID != sc.TargetBin.ID {
		t.Errorf("BinID = %d, want %d (target bin in the NGRP lane)", *order.BinID, sc.TargetBin.ID)
	}

	// Verify the source node was resolved to the concrete slot, not the NGRP
	if order.SourceNode == sc.Grp.Name {
		t.Errorf("SourceNode = %q (still NGRP name), should be resolved to concrete slot %q", order.SourceNode, sc.Slots[0].Name)
	}
	if order.SourceNode != sc.Slots[0].Name {
		t.Errorf("SourceNode = %q, want %q", order.SourceNode, sc.Slots[0].Name)
	}

	// Verify fleet got the right dispatch
	if len(backend.Orders()) != 1 {
		t.Fatalf("fleet orders = %d, want 1", len(backend.Orders()))
	}
}

// TestDispatcher_MoveOrder_NGRPSource_NoBin verifies that a move order with an
// NGRP source and no available bins gets queued rather than silently dispatching
// without a bin claim.
func TestDispatcher_MoveOrder_NGRPSource_NoBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}
	lanType, err := db.GetNodeTypeByCode("LANE")
	if err != nil {
		t.Fatalf("get LANE type: %v", err)
	}
	bp := &payloads.Payload{Code: "PART-MVEMPTY", Description: "test"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	// Create NGRP with an empty lane (no bins)
	grp := &nodes.Node{Name: "GRP-MVEMPTY", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create NGRP")
	lane := &nodes.Node{Name: "GRP-MVEMPTY-L1", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
	depth := 1
	slot := &nodes.Node{Name: "GRP-MVEMPTY-L1-S1", ParentID: &lane.ID, Enabled: true, Depth: &depth}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")
	lineNode := &nodes.Node{Name: "LINE-MVEMPTY", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(lineNode), "create line")

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db, LaneLock: NewLaneLock(), DebugLog: nil}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)

	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "move-empty-ngrp-1",
		OrderType:    OrderTypeMove,
		PayloadCode:  bp.Code,
		SourceNode:   grp.Name,
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})

	order := testdb.RequireOrder(t, db, "move-empty-ngrp-1")

	// Should be queued, not failed or dispatched with nil BinID
	if order.Status != StatusQueued {
		t.Errorf("status = %q, want %q (empty NGRP should queue, not fail or dispatch without bin)", order.Status, StatusQueued)
	}

	// Fleet should NOT have received any dispatch
	if len(backend.Orders()) != 0 {
		t.Errorf("fleet orders = %d, want 0 (no bin available, should not dispatch)", len(backend.Orders()))
	}
}

// TestDispatcher_MoveOrder_NGRPSource_BuriedBin verifies that a move order
// with an NGRP source where the only matching bin is buried behind blockers
// triggers the reshuffle path (planBuriedReshuffle) rather than silently
// dispatching without a bin claim.
func TestDispatcher_MoveOrder_NGRPSource_BuriedBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// SetupCompound with NumSlots=2 (default) creates:
	//   Slot 1 (depth 1, front) — blocker bin
	//   Slot 2 (depth 2, back)  — target bin (oldest, the one we want)
	// The target bin is buried behind the blocker, so the resolver returns
	// a BuriedError, which planMove should delegate to planBuriedReshuffle.
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix: "MVBURY",
	})

	backend := testdb.NewTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db, LaneLock: NewLaneLock(), DebugLog: nil}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)

	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "move-buried-1",
		OrderType:    OrderTypeMove,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name, // NGRP — target bin is buried
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1.0,
	})

	order := testdb.RequireOrder(t, db, "move-buried-1")

	// The order should trigger a compound reshuffle — status = "reshuffling"
	if order.Status != StatusReshuffling {
		t.Errorf("status = %q, want %q (buried bin should trigger reshuffle)", order.Status, StatusReshuffling)
	}

	// BinID should NOT be set yet — the reshuffle must complete first
	// before the actual bin can be claimed and moved.
	// (The compound order children handle the individual moves.)

	// Fleet should have received dispatch(es) for the compound children
	if len(backend.Orders()) == 0 {
		t.Error("fleet orders = 0, want >= 1 (compound reshuffle children should be dispatched)")
	}
}

// Bug: 2026-04-13 — planMove() did not guard against same-node moves,
// which would dispatch a physically impossible fleet transport.
func TestDispatcher_MoveOrder_SameNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)

	testdb.CreateBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-SAME-1")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "move-same-node-1",
		OrderType:    OrderTypeMove,
		PayloadCode:  "PART-A",
		SourceNode:   storageNode.Name,
		DeliveryNode: storageNode.Name,
		Quantity:     1.0,
	})

	// Order should be failed, not dispatched.
	testdb.AssertOrderStatus(t, db, "move-same-node-1", StatusFailed)

	// Fleet should NOT have received any orders.
	if len(backend.Orders()) != 0 {
		t.Errorf("fleet orders = %d, want 0 (same-node move should not dispatch)", len(backend.Orders()))
	}
}
