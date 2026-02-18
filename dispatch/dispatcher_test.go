package dispatch

import (
	"os"
	"path/filepath"
	"testing"

	"shingocore/config"
	"shingocore/messaging"
	"shingocore/rds"
	"shingocore/store"
)

// --- Mock emitter ---

type mockEmitter struct {
	received   []emitReceived
	dispatched []emitDispatched
	failed     []emitFailed
	cancelled  []emitCancelled
	completed  []emitCompleted
}

type emitReceived struct {
	orderID         int64
	payloadTypeCode string
}
type emitDispatched struct {
	orderID    int64
	rdsOrderID string
}
type emitFailed struct {
	orderID   int64
	errorCode string
}
type emitCancelled struct {
	orderID int64
	reason  string
}
type emitCompleted struct {
	orderID int64
}

func (m *mockEmitter) EmitOrderReceived(orderID int64, _, _, _, payloadTypeCode, _ string) {
	m.received = append(m.received, emitReceived{orderID, payloadTypeCode})
}
func (m *mockEmitter) EmitOrderDispatched(orderID int64, rdsOrderID, _, _ string) {
	m.dispatched = append(m.dispatched, emitDispatched{orderID, rdsOrderID})
}
func (m *mockEmitter) EmitOrderFailed(orderID int64, _, _, errorCode, _ string) {
	m.failed = append(m.failed, emitFailed{orderID, errorCode})
}
func (m *mockEmitter) EmitOrderCancelled(orderID int64, _, _, reason string) {
	m.cancelled = append(m.cancelled, emitCancelled{orderID, reason})
}
func (m *mockEmitter) EmitOrderCompleted(orderID int64, _, _ string) {
	m.completed = append(m.completed, emitCompleted{orderID})
}

// --- Test helpers ---

func testDB(t *testing.T) *store.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := store.Open(&config.DatabaseConfig{
		Driver: "sqlite",
		SQLite: config.SQLiteConfig{Path: dbPath},
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		os.Remove(dbPath)
	})
	return db
}

func setupTestData(t *testing.T, db *store.DB) (storageNode *store.Node, lineNode *store.Node, pt *store.PayloadType) {
	t.Helper()
	storageNode = &store.Node{Name: "STORAGE-A1", RDSLocation: "Loc-01", NodeType: "storage", Zone: "A", Capacity: 10, Enabled: true}
	if err := db.CreateNode(storageNode); err != nil {
		t.Fatalf("create storage node: %v", err)
	}
	lineNode = &store.Node{Name: "LINE1-IN", RDSLocation: "Loc-10", NodeType: "line_side", Capacity: 5, Enabled: true}
	if err := db.CreateNode(lineNode); err != nil {
		t.Fatalf("create line node: %v", err)
	}
	pt = &store.PayloadType{Name: "PART-A", Description: "Steel bracket tote", FormFactor: "tote", DefaultManifestJSON: "{}"}
	if err := db.CreatePayloadType(pt); err != nil {
		t.Fatalf("create payload type: %v", err)
	}
	return
}

func newTestDispatcher(t *testing.T, db *store.DB, rdsClient *rds.Client) (*Dispatcher, *mockEmitter) {
	t.Helper()
	emitter := &mockEmitter{}
	d := NewDispatcher(db, rdsClient, emitter, "plant-alpha", "shingocore/dispatch")
	return d, emitter
}

// --- Tests ---

func TestHandleOrderRequest_Retrieve_NoSource(t *testing.T) {
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	// No RDS client needed since it should fail before dispatch
	d, emitter := newTestDispatcher(t, db, rds.NewClient("http://localhost:1", 1))

	env := &messaging.Envelope{ClientID: "line-1", FactoryID: "plant-alpha"}
	req := messaging.OrderRequest{
		OrderUUID:       "uuid-1",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "PART-A",
		DeliveryNode:    lineNode.Name,
		Quantity:        1.0,
	}

	d.HandleOrderRequest(env, req)

	// Should emit received
	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	// Should fail because no available payloads exist
	if len(emitter.failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(emitter.failed))
	}
	if emitter.failed[0].errorCode != "no_source" {
		t.Errorf("error code = %q, want %q", emitter.failed[0].errorCode, "no_source")
	}
}

func TestHandleOrderRequest_Retrieve_InvalidDeliveryNode(t *testing.T) {
	db := testDB(t)
	setupTestData(t, db)

	d, emitter := newTestDispatcher(t, db, rds.NewClient("http://localhost:1", 1))

	env := &messaging.Envelope{ClientID: "line-1", FactoryID: "plant-alpha"}
	req := messaging.OrderRequest{
		OrderUUID:       "uuid-2",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "PART-A",
		DeliveryNode:    "NONEXISTENT",
		Quantity:        1.0,
	}

	d.HandleOrderRequest(env, req)

	// Should get an error reply enqueued (delivery node not found)
	if len(emitter.received) != 0 {
		t.Errorf("received events = %d, want 0 (should fail before order creation)", len(emitter.received))
	}
}

func TestHandleOrderRequest_Move_MissingPickup(t *testing.T) {
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	d, emitter := newTestDispatcher(t, db, rds.NewClient("http://localhost:1", 1))

	env := &messaging.Envelope{ClientID: "line-1", FactoryID: "plant-alpha"}
	req := messaging.OrderRequest{
		OrderUUID:       "uuid-3",
		OrderType:       OrderTypeMove,
		PayloadTypeCode: "PART-A",
		DeliveryNode:    lineNode.Name,
		PickupNode:      "",
		Quantity:        1.0,
	}

	d.HandleOrderRequest(env, req)

	if len(emitter.failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(emitter.failed))
	}
	if emitter.failed[0].errorCode != "missing_pickup" {
		t.Errorf("error code = %q, want %q", emitter.failed[0].errorCode, "missing_pickup")
	}
}

func TestHandleOrderRequest_Move_NoPayloadAtPickup(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)

	d, emitter := newTestDispatcher(t, db, rds.NewClient("http://localhost:1", 1))

	env := &messaging.Envelope{ClientID: "line-1", FactoryID: "plant-alpha"}
	req := messaging.OrderRequest{
		OrderUUID:       "uuid-4",
		OrderType:       OrderTypeMove,
		PayloadTypeCode: "PART-A",
		DeliveryNode:    lineNode.Name,
		PickupNode:      storageNode.Name,
		Quantity:        1.0,
	}

	d.HandleOrderRequest(env, req)

	// Should fail because no payloads at pickup
	if len(emitter.failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(emitter.failed))
	}
	if emitter.failed[0].errorCode != "no_payload" {
		t.Errorf("error code = %q, want %q", emitter.failed[0].errorCode, "no_payload")
	}
}

func TestHandleOrderRequest_UnknownType(t *testing.T) {
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	d, emitter := newTestDispatcher(t, db, rds.NewClient("http://localhost:1", 1))

	env := &messaging.Envelope{ClientID: "line-1", FactoryID: "plant-alpha"}
	req := messaging.OrderRequest{
		OrderUUID:       "uuid-5",
		OrderType:       "bogus",
		PayloadTypeCode: "PART-A",
		DeliveryNode:    lineNode.Name,
	}

	d.HandleOrderRequest(env, req)

	if len(emitter.failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(emitter.failed))
	}
	if emitter.failed[0].errorCode != "unknown_type" {
		t.Errorf("error code = %q, want %q", emitter.failed[0].errorCode, "unknown_type")
	}
}

func TestHandleOrderRequest_UnknownPayloadType(t *testing.T) {
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, rds.NewClient("http://localhost:1", 1))

	env := &messaging.Envelope{ClientID: "line-1", FactoryID: "plant-alpha"}
	req := messaging.OrderRequest{
		OrderUUID:       "uuid-pt",
		OrderType:       OrderTypeRetrieve,
		PayloadTypeCode: "NONEXISTENT",
		DeliveryNode:    lineNode.Name,
	}

	d.HandleOrderRequest(env, req)

	// Should fail before creating order — no received or failed events from emitter
	// but an error reply should be enqueued in the outbox
}

func TestHandleOrderCancel(t *testing.T) {
	db := testDB(t)

	order := &store.Order{EdgeUUID: "uuid-cancel", ClientID: "line-1", Status: StatusPending}
	db.CreateOrder(order)

	d, emitter := newTestDispatcher(t, db, rds.NewClient("http://localhost:1", 1))

	env := &messaging.Envelope{ClientID: "line-1", FactoryID: "plant-alpha"}
	req := messaging.OrderCancel{OrderUUID: "uuid-cancel", Reason: "operator cancelled"}

	d.HandleOrderCancel(env, req)

	if len(emitter.cancelled) != 1 {
		t.Fatalf("cancelled events = %d, want 1", len(emitter.cancelled))
	}
	if emitter.cancelled[0].reason != "operator cancelled" {
		t.Errorf("reason = %q, want %q", emitter.cancelled[0].reason, "operator cancelled")
	}

	// Verify order status updated
	got, _ := db.GetOrder(order.ID)
	if got.Status != StatusCancelled {
		t.Errorf("status = %q, want %q", got.Status, StatusCancelled)
	}
}

func TestHandleOrderCancel_UnclaimsPayloads(t *testing.T) {
	db := testDB(t)
	storageNode, _, pt := setupTestData(t, db)

	order := &store.Order{EdgeUUID: "uuid-unclaim", ClientID: "line-1", Status: StatusDispatched}
	db.CreateOrder(order)

	p := &store.Payload{PayloadTypeID: pt.ID, NodeID: &storageNode.ID, Status: "available"}
	db.CreatePayload(p)
	db.ClaimPayload(p.ID, order.ID)

	d, _ := newTestDispatcher(t, db, rds.NewClient("http://localhost:1", 1))

	env := &messaging.Envelope{ClientID: "line-1", FactoryID: "plant-alpha"}
	d.HandleOrderCancel(env, messaging.OrderCancel{OrderUUID: "uuid-unclaim", Reason: "test"})

	// Verify payload unclaimed
	got, _ := db.GetPayload(p.ID)
	if got.ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v, want nil", got.ClaimedBy)
	}
}

func TestHandleDeliveryReceipt(t *testing.T) {
	db := testDB(t)

	order := &store.Order{EdgeUUID: "uuid-receipt", ClientID: "line-1", Status: StatusDelivered}
	db.CreateOrder(order)

	d, emitter := newTestDispatcher(t, db, rds.NewClient("http://localhost:1", 1))

	env := &messaging.Envelope{ClientID: "line-1", FactoryID: "plant-alpha"}
	req := messaging.DeliveryReceipt{OrderUUID: "uuid-receipt", ReceiptType: "confirmed", FinalCount: 50}

	d.HandleDeliveryReceipt(env, req)

	if len(emitter.completed) != 1 {
		t.Fatalf("completed events = %d, want 1", len(emitter.completed))
	}

	// Verify order is completed
	got, _ := db.GetOrder(order.ID)
	if got.Status != StatusCompleted {
		t.Errorf("status = %q, want %q", got.Status, StatusCompleted)
	}
}

func TestFIFOPayloadSourceSelection(t *testing.T) {
	db := testDB(t)
	storageNode, _, pt := setupTestData(t, db)

	// Create another storage node
	s2 := &store.Node{Name: "STORAGE-B1", RDSLocation: "Loc-02", NodeType: "storage", Capacity: 10, Enabled: true}
	db.CreateNode(s2)

	// Older available payload at storageNode
	p1 := &store.Payload{PayloadTypeID: pt.ID, NodeID: &storageNode.ID, Status: "available"}
	db.CreatePayload(p1)
	// Newer available payload at s2
	p2 := &store.Payload{PayloadTypeID: pt.ID, NodeID: &s2.ID, Status: "available"}
	db.CreatePayload(p2)

	// FIFO should select oldest (p1) first
	source, err := db.FindSourcePayloadFIFO("PART-A")
	if err != nil {
		t.Fatalf("FindSourcePayloadFIFO: %v", err)
	}
	if source.ID != p1.ID {
		t.Errorf("source payload = %d, want %d (FIFO order)", source.ID, p1.ID)
	}
}

func TestStatusConstants(t *testing.T) {
	// Verify all plan-defined statuses exist
	statuses := []string{
		StatusPending, StatusSourcing, StatusDispatched, StatusInTransit,
		StatusDelivered, StatusConfirmed, StatusCompleted, StatusFailed, StatusCancelled,
	}
	expected := []string{
		"pending", "sourcing", "dispatched", "in_transit",
		"delivered", "confirmed", "completed", "failed", "cancelled",
	}
	for i, s := range statuses {
		if s != expected[i] {
			t.Errorf("status[%d] = %q, want %q", i, s, expected[i])
		}
	}
}

func TestOrderTypeConstants(t *testing.T) {
	if OrderTypeRetrieve != "retrieve" {
		t.Errorf("OrderTypeRetrieve = %q", OrderTypeRetrieve)
	}
	if OrderTypeMove != "move" {
		t.Errorf("OrderTypeMove = %q", OrderTypeMove)
	}
	if OrderTypeStore != "store" {
		t.Errorf("OrderTypeStore = %q", OrderTypeStore)
	}
}
