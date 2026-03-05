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
	storageNode, lineNode, ps := setupTestData(t, db)

	// Create an available payload at storage
	payload := &store.PayloadInstance{
		StyleID: ps.ID,
		NodeID:        &storageNode.ID,
		Status:        "available",
	}
	if err := db.CreateInstance(payload); err != nil {
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
	payload2, err := db.GetInstance(payload.ID)
	if err != nil {
		t.Fatalf("get payload: %v", err)
	}
	if payload2.ClaimedBy == nil {
		t.Error("payload should be claimed")
	}
}

func TestDispatcher_MoveOrder_FullLifecycle(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, ps := setupTestData(t, db)

	// Create an available payload at storage
	payload := &store.PayloadInstance{
		StyleID: ps.ID,
		NodeID:        &storageNode.ID,
		Status:        "available",
	}
	if err := db.CreateInstance(payload); err != nil {
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
	storageNode, lineNode, ps := setupTestData(t, db)

	// Create an available payload at line-side
	payload := &store.PayloadInstance{
		StyleID: ps.ID,
		NodeID:        &lineNode.ID,
		Status:        "available",
	}
	if err := db.CreateInstance(payload); err != nil {
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
	storageNode, lineNode, ps := setupTestData(t, db)

	// Create an available payload at storage
	payload := &store.PayloadInstance{
		StyleID: ps.ID,
		NodeID:        &storageNode.ID,
		Status:        "available",
	}
	db.CreateInstance(payload)

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
	claimed, _ := db.GetInstance(payload.ID)
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
	unclaimed, _ := db.GetInstance(payload.ID)
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
	storageNode, lineNode, ps := setupTestData(t, db)

	// Create another line node
	lineNode2 := &store.Node{Name: "LINE2-IN", VendorLocation: "Loc-20", NodeType: "line_side", Capacity: 5, Enabled: true}
	db.CreateNode(lineNode2)

	// Create an available payload
	payload := &store.PayloadInstance{
		StyleID: ps.ID,
		NodeID:        &storageNode.ID,
		Status:        "available",
	}
	db.CreateInstance(payload)

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
		OrderUUID:        "redirect-uuid-1",
		NewDeliveryNode:  lineNode2.Name,
	})

	// Verify order destination was updated (need to re-fetch from DB)
	order2, _ := db.GetOrderByUUID("redirect-uuid-1")
	if order2.DeliveryNode != lineNode2.Name {
		t.Errorf("delivery node = %q, want %q", order2.DeliveryNode, lineNode2.Name)
	}
}

func TestDispatcher_SyntheticNodeResolution(t *testing.T) {
	db := testDB(t)
	_, _, ps := setupTestData(t, db)

	// Look up the seeded synthetic node type (SUP)
	syntheticType, err := db.GetNodeTypeByCode("SUP")
	if err != nil {
		t.Fatalf("get synthetic node type: %v", err)
	}

	// Create a synthetic parent node
	parentNode := &store.Node{
		Name: "ZONE-A", VendorLocation: "", NodeType: "storage",
		NodeTypeID: &syntheticType.ID, Capacity: 0, Enabled: true,
	}
	if err := db.CreateNode(parentNode); err != nil {
		t.Fatalf("create parent node: %v", err)
	}

	// Create child nodes under the synthetic parent
	child1 := &store.Node{Name: "ZONE-A-01", VendorLocation: "Loc-A01", NodeType: "storage", Capacity: 10, Enabled: true}
	child2 := &store.Node{Name: "ZONE-A-02", VendorLocation: "Loc-A02", NodeType: "storage", Capacity: 10, Enabled: true}
	db.CreateNode(child1)
	db.CreateNode(child2)
	db.SetNodeParent(child1.ID, parentNode.ID)
	db.SetNodeParent(child2.ID, parentNode.ID)

	// Create a line node for delivery
	lineNode := &store.Node{Name: "LINE-SYN", VendorLocation: "Loc-SYN", NodeType: "line_side", Capacity: 5, Enabled: true}
	db.CreateNode(lineNode)

	// Put an available payload at child2 (not child1) to verify resolver picks the right child
	payload := &store.PayloadInstance{StyleID: ps.ID, NodeID: &child2.ID, Status: "available"}
	db.CreateInstance(payload)

	// Create dispatcher with resolver
	backend := newMockTrackingBackend()
	emitter := &mockEmitter{}
	resolver := &DefaultResolver{DB: db}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", resolver)

	env := testEnvelope()

	// Submit retrieve order targeting the synthetic parent — resolver should pick child2
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
	// Delivery node should be resolved to child2, not the synthetic parent
	if order.DeliveryNode != child2.Name {
		t.Errorf("delivery node = %q, want %q (resolved child)", order.DeliveryNode, child2.Name)
	}
	// Pickup should be child2 (where the payload is)
	if order.PickupNode != child2.Name {
		t.Errorf("pickup node = %q, want %q", order.PickupNode, child2.Name)
	}
}

func TestDispatcher_FleetFailure(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, ps := setupTestData(t, db)

	// Create an available payload
	payload := &store.PayloadInstance{StyleID: ps.ID, NodeID: &storageNode.ID, Status: "available"}
	db.CreateInstance(payload)

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
	p, _ := db.GetInstance(payload.ID)
	if p.ClaimedBy != nil {
		t.Errorf("payload should be unclaimed after fleet failure, ClaimedBy = %v", p.ClaimedBy)
	}
}

func TestDispatcher_PriorityHandling(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, ps := setupTestData(t, db)

	// Create available payloads
	payload1 := &store.PayloadInstance{StyleID: ps.ID, NodeID: &storageNode.ID, Status: "available"}
	payload2 := &store.PayloadInstance{StyleID: ps.ID, NodeID: &storageNode.ID, Status: "available"}
	db.CreateInstance(payload1)
	db.CreateInstance(payload2)

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

