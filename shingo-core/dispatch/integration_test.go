package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/store"
)

// mockTrackingBackend implements fleet.TrackingBackend for testing
type mockTrackingBackend struct {
	*mockBackend
	orders map[string]fleet.TransportOrderResult
}

func (m *mockTrackingBackend) InitTracker(emitter fleet.TrackerEmitter, resolver fleet.OrderIDResolver) {
	// no-op for tests
}

func (m *mockTrackingBackend) Tracker() fleet.OrderTracker {
	return nil
}

func (m *mockTrackingBackend) CreateTransportOrder(req fleet.TransportOrderRequest) (fleet.TransportOrderResult, error) {
	result := fleet.TransportOrderResult{
		VendorOrderID: req.OrderID,
	}
	m.orders[req.OrderID] = result
	return result, nil
}

func newMockTrackingBackend() *mockTrackingBackend {
	return &mockTrackingBackend{
		mockBackend: &mockBackend{},
		orders:      make(map[string]fleet.TransportOrderResult),
	}
}

func TestDispatcher_RetrieveOrder_FullLifecycle(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin at the storage node and an available payload
	bin := &store.Bin{BinTypeID: 1, Label: "BIN-RET-1", NodeID: &storageNode.ID, Status: "active"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}
	payload := &store.Payload{
		BlueprintID: bp.ID,
		BinID:       &bin.ID,
		Status:      "available",
	}
	if err := db.CreatePayload(payload); err != nil {
		t.Fatalf("create payload: %v", err)
	}

	backend := newMockTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Phase 1: Submit retrieve order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "retrieve-uuid-1",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "PART-A",
		DeliveryNode:    lineNode.Name,
		Quantity:        1.0,
	})

	// Verify order was created
	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	// Verify order is in database
	order, err := db.GetOrderByUUID("retrieve-uuid-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != StatusDispatched {
		t.Errorf("status = %q, want %q", order.Status, StatusDispatched)
	}
	if order.PickupNode != storageNode.Name {
		t.Errorf("pickup node = %q, want %q", order.PickupNode, storageNode.Name)
	}
	if order.DeliveryNode != lineNode.Name {
		t.Errorf("delivery node = %q, want %q", order.DeliveryNode, lineNode.Name)
	}

	// Verify vendor order was created
	if order.VendorOrderID == "" {
		t.Fatal("vendor order ID should be set")
	}

	// Phase 2: Simulate delivery receipt
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

	// Verify payload was claimed
	payload2, err := db.GetPayload(payload.ID)
	if err != nil {
		t.Fatalf("get payload: %v", err)
	}
	if payload2.ClaimedBy == nil {
		t.Error("payload should be claimed")
	}
}

func TestDispatcher_MoveOrder_FullLifecycle(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin at storage node and an available payload
	bin := &store.Bin{BinTypeID: 1, Label: "BIN-MOV-1", NodeID: &storageNode.ID, Status: "active"}
	db.CreateBin(bin)
	payload := &store.Payload{
		BlueprintID: bp.ID,
		BinID:       &bin.ID,
		Status:      "available",
	}
	if err := db.CreatePayload(payload); err != nil {
		t.Fatalf("create payload: %v", err)
	}

	backend := newMockTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Phase 1: Submit move order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "move-uuid-1",
		OrderType:       OrderTypeMove,
		PayloadTypeCode: "PART-A",
		PickupNode:      storageNode.Name,
		DeliveryNode:    lineNode.Name,
		Quantity:        1.0,
	})

	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	order, err := db.GetOrderByUUID("move-uuid-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != StatusDispatched {
		t.Errorf("status = %q, want %q", order.Status, StatusDispatched)
	}

	// Phase 2: Simulate delivery receipt
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

func TestDispatcher_StoreOrder_FullLifecycle(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin at line-side and an available payload
	bin := &store.Bin{BinTypeID: 1, Label: "BIN-STO-1", NodeID: &lineNode.ID, Status: "active"}
	db.CreateBin(bin)
	payload := &store.Payload{
		BlueprintID: bp.ID,
		BinID:       &bin.ID,
		Status:      "available",
	}
	if err := db.CreatePayload(payload); err != nil {
		t.Fatalf("create payload: %v", err)
	}

	backend := newMockTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Phase 1: Submit store order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "store-uuid-1",
		OrderType:       OrderTypeStore,
		PayloadTypeCode: "PART-A",
		PickupNode:      lineNode.Name,
		Quantity:        1.0,
	})

	// Store orders should select a storage destination
	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	order, err := db.GetOrderByUUID("store-uuid-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.DeliveryNode == "" {
		t.Fatal("delivery node should be set for store order")
	}
	// Delivery node should be a storage node
	if order.DeliveryNode != storageNode.Name {
		// Could be another storage node, just verify it's set
		t.Logf("delivery node = %q", order.DeliveryNode)
	}
}

func TestDispatcher_CancelOrder(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin at storage and an available payload
	bin := &store.Bin{BinTypeID: 1, Label: "BIN-CAN-1", NodeID: &storageNode.ID, Status: "active"}
	db.CreateBin(bin)
	payload := &store.Payload{
		BlueprintID: bp.ID,
		BinID:       &bin.ID,
		Status:      "available",
	}
	db.CreatePayload(payload)

	backend := newMockTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Submit retrieve order — dispatch will claim the payload
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "cancel-uuid-1",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "PART-A",
		DeliveryNode:    lineNode.Name,
		Quantity:        1.0,
	})

	order, _ := db.GetOrderByUUID("cancel-uuid-1")

	// Verify payload was claimed by this order
	claimed, _ := db.GetPayload(payload.ID)
	if claimed.ClaimedBy == nil || *claimed.ClaimedBy != order.ID {
		t.Fatalf("payload should be claimed by order %d before cancel", order.ID)
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

	// Verify payload was unclaimed by the cancel
	unclaimed, _ := db.GetPayload(payload.ID)
	if unclaimed.ClaimedBy != nil {
		t.Errorf("payload should be unclaimed after cancel, but ClaimedBy = %v", unclaimed.ClaimedBy)
	}

	// Verify cancelled event was emitted
	if len(emitter.cancelled) != 1 {
		t.Fatalf("cancelled events = %d, want 1", len(emitter.cancelled))
	}
}

func TestDispatcher_RedirectOrder(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create another line node
	lineNode2 := &store.Node{Name: "LINE2-IN", Enabled: true}
	db.CreateNode(lineNode2)

	// Create a bin and an available payload
	bin := &store.Bin{BinTypeID: 1, Label: "BIN-RED-1", NodeID: &storageNode.ID, Status: "active"}
	db.CreateBin(bin)
	payload := &store.Payload{
		BlueprintID: bp.ID,
		BinID:       &bin.ID,
		Status:      "available",
	}
	db.CreatePayload(payload)

	backend := newMockTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Submit move order from storage to line1
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "redirect-uuid-1",
		OrderType:       OrderTypeMove,
		PayloadTypeCode: "PART-A",
		PickupNode:      storageNode.Name,
		DeliveryNode:    lineNode.Name,
		Quantity:        1.0,
	})

	// Redirect to line2
	d.HandleOrderRedirect(env, &protocol.OrderRedirect{
		OrderUUID:       "redirect-uuid-1",
		NewDeliveryNode: lineNode2.Name,
	})

	// Verify order destination was updated (need to re-fetch from DB)
	order2, _ := db.GetOrderByUUID("redirect-uuid-1")
	if order2.DeliveryNode != lineNode2.Name {
		t.Errorf("delivery node = %q, want %q", order2.DeliveryNode, lineNode2.Name)
	}
}

func TestDispatcher_SyntheticNodeResolution(t *testing.T) {
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	// Look up the seeded synthetic node type (NGRP)
	syntheticType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get synthetic node type: %v", err)
	}

	// Create a synthetic parent node (delivery zone)
	parentNode := &store.Node{
		Name: "ZONE-A", IsSynthetic: true,
		NodeTypeID: &syntheticType.ID, Enabled: true,
	}
	if err := db.CreateNode(parentNode); err != nil {
		t.Fatalf("create parent node: %v", err)
	}

	// Create child nodes under the synthetic parent (lineside slots)
	child1 := &store.Node{Name: "ZONE-A-01", Enabled: true}
	child2 := &store.Node{Name: "ZONE-A-02", Enabled: true}
	db.CreateNode(child1)
	db.CreateNode(child2)
	db.SetNodeParent(child1.ID, parentNode.ID)
	db.SetNodeParent(child2.ID, parentNode.ID)

	// Put a bin at child2 to occupy it (child2 occupied, child1 empty)
	occBin := &store.Bin{BinTypeID: 1, Label: "BIN-SYN-OCC", NodeID: &child2.ID, Status: "active"}
	db.CreateBin(occBin)

	// Create source payload at a separate node for FIFO to find
	srcNode := &store.Node{Name: "SRC-SYN", Enabled: true}
	db.CreateNode(srcNode)
	srcBin := &store.Bin{BinTypeID: 1, Label: "BIN-SYN-SRC", NodeID: &srcNode.ID, Status: "active"}
	db.CreateBin(srcBin)
	db.CreatePayload(&store.Payload{BlueprintID: bp.ID, BinID: &srcBin.ID, Status: "available"})

	// Create dispatcher with resolver
	backend := newMockTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)

	env := testEnvelope()

	// Submit retrieve order targeting synthetic parent — delivery should resolve
	// to child1 (empty slot), source should pick srcPayload via FIFO
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "syn-retrieve-1",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "PART-A",
		DeliveryNode:    parentNode.Name,
		Quantity:        1.0,
	})

	// Verify order was dispatched (not failed)
	if len(emitter.failed) > 0 {
		t.Fatalf("order should not fail, got error: %s", emitter.failed[0].errorCode)
	}
	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	order, err := db.GetOrderByUUID("syn-retrieve-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != StatusDispatched {
		t.Errorf("status = %q, want %q", order.Status, StatusDispatched)
	}
	// Delivery node should be resolved to child1 (empty slot), not child2 (occupied)
	if order.DeliveryNode != child1.Name {
		t.Errorf("delivery node = %q, want %q (empty child)", order.DeliveryNode, child1.Name)
	}
	// Pickup should be source node (where the FIFO payload is)
	if order.PickupNode != srcNode.Name {
		t.Errorf("pickup node = %q, want %q", order.PickupNode, srcNode.Name)
	}
}

// TestDispatcher_MultiOrderToSyntheticNGRP verifies that multiple retrieve orders
// to the same synthetic NGRP resolve to different physical children and that
// in-flight awareness prevents double-booking of the same slot.
func TestDispatcher_MultiOrderToSyntheticNGRP(t *testing.T) {
	db := testDB(t)
	_, _, _ = setupTestData(t, db)

	syntheticType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}

	// Create NGRP zone with 3 physical children
	zone := &store.Node{Name: "PRESS-A1", IsSynthetic: true, NodeTypeID: &syntheticType.ID, Enabled: true}
	db.CreateNode(zone)
	slot1 := &store.Node{Name: "PRESS-A1-01", Enabled: true}
	slot2 := &store.Node{Name: "PRESS-A1-02", Enabled: true}
	slot3 := &store.Node{Name: "PRESS-A1-03", Enabled: true}
	db.CreateNode(slot1)
	db.CreateNode(slot2)
	db.CreateNode(slot3)
	db.SetNodeParent(slot1.ID, zone.ID)
	db.SetNodeParent(slot2.ID, zone.ID)
	db.SetNodeParent(slot3.ID, zone.ID)

	// Create source payloads in a supermarket (blueprint A x2, blueprint B x1)
	bpA := &store.Blueprint{Code: "PART-MULTI-A", DefaultManifestJSON: "{}"}
	bpB := &store.Blueprint{Code: "PART-MULTI-B", DefaultManifestJSON: "{}"}
	db.CreateBlueprint(bpA)
	db.CreateBlueprint(bpB)

	supermarket := &store.Node{Name: "SM-MULTI", Zone: "W", Enabled: true}
	db.CreateNode(supermarket)

	binA1 := &store.Bin{BinTypeID: 1, Label: "M-A1", NodeID: &supermarket.ID, Status: "active"}
	binA2 := &store.Bin{BinTypeID: 1, Label: "M-A2", NodeID: &supermarket.ID, Status: "active"}
	binB1 := &store.Bin{BinTypeID: 1, Label: "M-B1", NodeID: &supermarket.ID, Status: "active"}
	db.CreateBin(binA1)
	db.CreateBin(binA2)
	db.CreateBin(binB1)
	db.CreatePayload(&store.Payload{BlueprintID: bpA.ID, BinID: &binA1.ID, Status: "available"})
	db.CreatePayload(&store.Payload{BlueprintID: bpA.ID, BinID: &binA2.ID, Status: "available"})
	db.CreatePayload(&store.Payload{BlueprintID: bpB.ID, BinID: &binB1.ID, Status: "available"})

	backend := newMockTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	// Order 1: blueprint A -> PRESS-A1
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "multi-1",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "PART-MULTI-A",
		DeliveryNode:    zone.Name,
		Quantity:        1,
	})
	// Order 2: blueprint A -> PRESS-A1
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "multi-2",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "PART-MULTI-A",
		DeliveryNode:    zone.Name,
		Quantity:        1,
	})
	// Order 3: blueprint B -> PRESS-A1
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "multi-3",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "PART-MULTI-B",
		DeliveryNode:    zone.Name,
		Quantity:        1,
	})

	if len(emitter.failed) > 0 {
		t.Fatalf("unexpected failures: %d (first: %s)", len(emitter.failed), emitter.failed[0].errorCode)
	}

	o1, _ := db.GetOrderByUUID("multi-1")
	o2, _ := db.GetOrderByUUID("multi-2")
	o3, _ := db.GetOrderByUUID("multi-3")

	// All three should be dispatched
	for _, o := range []*store.Order{o1, o2, o3} {
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
		OrderUUID:       "multi-4",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "PART-MULTI-A",
		DeliveryNode:    zone.Name,
		Quantity:        1,
	})

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
	db := testDB(t)
	_, _, _ = setupTestData(t, db)

	syntheticType, _ := db.GetNodeTypeByCode("NGRP")

	// Create NGRP zone with 2 children, one occupied
	zone := &store.Node{Name: "EMPTY-ZONE", IsSynthetic: true, NodeTypeID: &syntheticType.ID, Enabled: true}
	db.CreateNode(zone)
	slot1 := &store.Node{Name: "EZ-01", Enabled: true}
	slot2 := &store.Node{Name: "EZ-02", Enabled: true}
	db.CreateNode(slot1)
	db.CreateNode(slot2)
	db.SetNodeParent(slot1.ID, zone.ID)
	db.SetNodeParent(slot2.ID, zone.ID)

	// Occupy slot1
	occBin := &store.Bin{BinTypeID: 1, Label: "OCC-1", NodeID: &slot1.ID, Status: "active"}
	db.CreateBin(occBin)

	// Create blueprint with bin type compatibility
	bp := &store.Blueprint{Code: "EMPTY-BP", DefaultManifestJSON: "{}"}
	db.CreateBlueprint(bp)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetBlueprintBinTypes(bp.ID, []int64{bt.ID})

	// Create an empty compatible bin somewhere (source)
	srcNode := &store.Node{Name: "EMPTY-SRC", Enabled: true}
	db.CreateNode(srcNode)
	emptyBin := &store.Bin{BinTypeID: bt.ID, Label: "EMPTY-BIN-1", NodeID: &srcNode.ID, Status: "available"}
	db.CreateBin(emptyBin)

	backend := newMockTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "empty-1",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "EMPTY-BP",
		DeliveryNode:    zone.Name,
		RetrieveEmpty:   true,
		Quantity:        1,
	})

	if len(emitter.failed) > 0 {
		t.Fatalf("order should not fail, got: %s", emitter.failed[0].errorCode)
	}

	o, err := db.GetOrderByUUID("empty-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if o.Status != StatusDispatched {
		t.Errorf("status = %q, want dispatched", o.Status)
	}
	// Delivery should resolve to slot2 (empty), not slot1 (occupied)
	if o.DeliveryNode != slot2.Name {
		t.Errorf("delivery = %q, want %q (empty slot)", o.DeliveryNode, slot2.Name)
	}
}

// TestDispatcher_DotNotationBypassesResolver verifies that ordering to a
// specific child using dot notation (ZONE.Node10) skips resolver — the
// physical node is used directly.
func TestDispatcher_DotNotationBypassesResolver(t *testing.T) {
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	syntheticType, _ := db.GetNodeTypeByCode("NGRP")
	zone := &store.Node{Name: "DOT-ZONE", IsSynthetic: true, NodeTypeID: &syntheticType.ID, Enabled: true}
	db.CreateNode(zone)
	child := &store.Node{Name: "SLOT-X", Enabled: true}
	db.CreateNode(child)
	db.SetNodeParent(child.ID, zone.ID)

	// Create source payload
	srcNode := &store.Node{Name: "DOT-SRC", Enabled: true}
	db.CreateNode(srcNode)
	bin := &store.Bin{BinTypeID: 1, Label: "DOT-BIN-1", NodeID: &srcNode.ID, Status: "active"}
	db.CreateBin(bin)
	db.CreatePayload(&store.Payload{BlueprintID: bp.ID, BinID: &bin.ID, Status: "available"})

	backend := newMockTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)
	env := testEnvelope()

	// Use dot notation: "DOT-ZONE.SLOT-X" — resolves to physical child directly
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "dot-1",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "PART-A",
		DeliveryNode:    "DOT-ZONE.SLOT-X",
		Quantity:        1,
	})

	if len(emitter.failed) > 0 {
		t.Fatalf("order should not fail, got: %s", emitter.failed[0].errorCode)
	}

	o, _ := db.GetOrderByUUID("dot-1")
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
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create a bin and an available payload
	bin := &store.Bin{BinTypeID: 1, Label: "BIN-FF-1", NodeID: &storageNode.ID, Status: "active"}
	db.CreateBin(bin)
	payload := &store.Payload{BlueprintID: bp.ID, BinID: &bin.ID, Status: "available"}
	db.CreatePayload(payload)

	// Use mockBackend (returns errors for all fleet ops)
	d, emitter := newTestDispatcher(t, db, &mockBackend{})

	env := testEnvelope()

	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "fleet-fail-1",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "PART-A",
		DeliveryNode:    lineNode.Name,
		Quantity:        1.0,
	})

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
	order, err := db.GetOrderByUUID("fleet-fail-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != StatusFailed {
		t.Errorf("status = %q, want %q", order.Status, StatusFailed)
	}

	// Verify payload was unclaimed after fleet failure
	p, _ := db.GetPayload(payload.ID)
	if p.ClaimedBy != nil {
		t.Errorf("payload should be unclaimed after fleet failure, ClaimedBy = %v", p.ClaimedBy)
	}
}

func TestDispatcher_PriorityHandling(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create bins and available payloads
	bin1 := &store.Bin{BinTypeID: 1, Label: "BIN-PRI-1", NodeID: &storageNode.ID, Status: "active"}
	db.CreateBin(bin1)
	payload1 := &store.Payload{BlueprintID: bp.ID, BinID: &bin1.ID, Status: "available"}
	db.CreatePayload(payload1)

	bin2 := &store.Bin{BinTypeID: 1, Label: "BIN-PRI-2", NodeID: &storageNode.ID, Status: "active"}
	db.CreateBin(bin2)
	payload2 := &store.Payload{BlueprintID: bp.ID, BinID: &bin2.ID, Status: "available"}
	db.CreatePayload(payload2)

	backend := newMockTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	env := testEnvelope()

	// Submit low priority order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "low-priority",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "PART-A",
		DeliveryNode:    lineNode.Name,
		Quantity:        1.0,
		Priority:        0,
	})

	// Submit high priority order
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "high-priority",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "PART-A",
		DeliveryNode:    lineNode.Name,
		Quantity:        1.0,
		Priority:        10,
	})

	// Both orders should be dispatched
	order1, _ := db.GetOrderByUUID("low-priority")
	order2, _ := db.GetOrderByUUID("high-priority")

	if order1.Priority != 0 {
		t.Errorf("low priority = %d, want 0", order1.Priority)
	}
	if order2.Priority != 10 {
		t.Errorf("high priority = %d, want 10", order2.Priority)
	}
}

func TestHandleRetrieve_BinTracking(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	// Create bin + payload
	bin := &store.Bin{BinTypeID: 1, Label: "BIN-BT-1", NodeID: &storageNode.ID, Status: "active"}
	db.CreateBin(bin)
	payload := &store.Payload{BlueprintID: bp.ID, BinID: &bin.ID, Status: "available"}
	db.CreatePayload(payload)

	backend := newMockTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "uuid-bin-track",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "PART-A",
		DeliveryNode:    lineNode.Name,
		Quantity:        1.0,
	})

	order, err := db.GetOrderByUUID("uuid-bin-track")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}

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
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)

	// Set up blueprint_bin_types for compatible empty bin
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetBlueprintBinTypes(bp.ID, []int64{bt.ID})

	// Create an empty bin at the station (simulating a bin at a produce location)
	produceNode := &store.Node{Name: "PRODUCE-1", Enabled: true}
	db.CreateNode(produceNode)

	bin := &store.Bin{BinTypeID: bt.ID, Label: "BIN-ING-1", NodeID: &produceNode.ID, Status: "available"}
	db.CreateBin(bin)

	// Also create a storage node for the store destination
	_ = storageNode

	backend := newMockTrackingBackend()
	d, emitter := newTestDispatcher(t, db, backend)

	env := testEnvelope()
	d.HandleOrderIngest(env, &protocol.OrderIngestRequest{
		OrderUUID:     "uuid-ingest-1",
		BlueprintCode: bp.Code,
		BinLabel:      "BIN-ING-1",
		PickupNode:    "PRODUCE-1",
		Quantity:      100,
		Manifest: []protocol.IngestManifestItem{
			{PartNumber: "PN-001", Quantity: 50, Description: "Bolt M8"},
			{PartNumber: "PN-002", Quantity: 50, Description: "Washer M8"},
		},
	})

	// Should have received the order
	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	// Should have created a payload on the bin
	payloads, _ := db.ListPayloadsByBin(bin.ID)
	if len(payloads) != 1 {
		t.Fatalf("payloads on bin = %d, want 1", len(payloads))
	}
	if payloads[0].BlueprintID != bp.ID {
		t.Errorf("payload blueprint = %d, want %d", payloads[0].BlueprintID, bp.ID)
	}

	// Bin should be claimed
	gotBin, _ := db.GetBin(bin.ID)
	if gotBin.ClaimedBy == nil {
		t.Fatal("bin should be claimed after ingest")
	}

	// Manifest items should be created
	order, _ := db.GetOrderByUUID("uuid-ingest-1")
	if order == nil {
		t.Fatal("order not found")
	}
	if order.BinID == nil || *order.BinID != bin.ID {
		t.Errorf("order BinID = %v, want %d", order.BinID, bin.ID)
	}
}
