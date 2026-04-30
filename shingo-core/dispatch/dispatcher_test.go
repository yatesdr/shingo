//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// --- Mock emitter ---

type mockEmitter struct {
	received   []emitReceived
	dispatched []emitDispatched
	failed     []emitFailed
	cancelled  []emitCancelled
	completed  []emitCompleted
	queued     []emitQueued
}

type emitReceived struct {
	orderID     int64
	payloadCode string
}
type emitDispatched struct {
	orderID       int64
	vendorOrderID string
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
type emitQueued struct {
	orderID int64
}

func (m *mockEmitter) EmitOrderReceived(orderID int64, _, _ string, _ protocol.OrderType, payloadCode, _ string) {
	m.received = append(m.received, emitReceived{orderID, payloadCode})
}
func (m *mockEmitter) EmitOrderDispatched(orderID int64, vendorOrderID, _, _ string) {
	m.dispatched = append(m.dispatched, emitDispatched{orderID, vendorOrderID})
}
func (m *mockEmitter) EmitOrderFailed(orderID int64, _, _, errorCode, _ string) {
	m.failed = append(m.failed, emitFailed{orderID, errorCode})
}
func (m *mockEmitter) EmitOrderCancelled(orderID int64, _, _, reason, _ string) {
	m.cancelled = append(m.cancelled, emitCancelled{orderID, reason})
}
func (m *mockEmitter) EmitOrderCompleted(orderID int64, _, _ string) {
	m.completed = append(m.completed, emitCompleted{orderID})
}
func (m *mockEmitter) EmitOrderQueued(orderID int64, _, _, _ string) {
	m.queued = append(m.queued, emitQueued{orderID})
}

// --- Test helpers (thin wrappers delegating to internal/testdb) ---

func testDB(t *testing.T) *store.DB {
	return testdb.Open(t)
}

func setupTestData(t *testing.T, db *store.DB) (storageNode *nodes.Node, lineNode *nodes.Node, bp *payloads.Payload) {
	t.Helper()
	sd := testdb.SetupStandardData(t, db)
	return sd.StorageNode, sd.LineNode, sd.Payload
}


func TestHandleOrderReceipt_DuplicateCompletedOrderIgnored(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	order := &orders.Order{
		EdgeUUID:     "dup-receipt",
		StationID:    "edge.line1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusDelivered,
		Quantity:     1,
		DeliveryNode: lineNode.Name,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.CompleteOrder(order.ID); err != nil {
		t.Fatalf("complete order: %v", err)
	}

	emitter := &mockEmitter{}
	d := NewDispatcher(db, testdb.NewFailingBackend(), emitter, "core", "dispatch", nil)
	env := &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: order.StationID}}

	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   order.EdgeUUID,
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	if len(emitter.completed) != 0 {
		t.Fatalf("expected no completion event for duplicate receipt, got %d", len(emitter.completed))
	}
}

func newTestDispatcher(t *testing.T, db *store.DB, backend fleet.Backend) (*Dispatcher, *mockEmitter) {
	t.Helper()
	emitter := &mockEmitter{}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", nil)
	return d, emitter
}

func testEnvelope() *protocol.Envelope {
	return testdb.Envelope()
}

// --- Tests ---

func TestHandleOrderRequest_Retrieve_NoSource(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	// No fleet backend needed since it should fail before dispatch
	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "uuid-1",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})

	// Should emit received
	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}

	// Should queue because no available payloads exist (queued fulfillment)
	if len(emitter.queued) != 1 {
		t.Fatalf("queued events = %d, want 1", len(emitter.queued))
	}
}

func TestHandleOrderRequest_Retrieve_InvalidDeliveryNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)

	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "uuid-2",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "PART-A",
		DeliveryNode: "NONEXISTENT",
		Quantity:     1.0,
	})

	// Should get an error reply enqueued (delivery node not found)
	if len(emitter.received) != 0 {
		t.Errorf("received events = %d, want 0 (should fail before order creation)", len(emitter.received))
	}
}

func TestHandleOrderRequest_Move_MissingPickup(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "uuid-3",
		OrderType:    OrderTypeMove,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		SourceNode:   "",
		Quantity:     1.0,
	})

	if len(emitter.failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(emitter.failed))
	}
	if emitter.failed[0].errorCode != "missing_source" {
		t.Errorf("error code = %q, want %q", emitter.failed[0].errorCode, "missing_source")
	}
}

func TestHandleOrderRequest_Move_NoPayloadAtPickup(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)

	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "uuid-4",
		OrderType:    OrderTypeMove,
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
		SourceNode:   storageNode.Name,
		Quantity:     1.0,
	})

	// Should fail because no payloads at pickup
	if len(emitter.failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(emitter.failed))
	}
	if emitter.failed[0].errorCode != "no_payload" {
		t.Errorf("error code = %q, want %q", emitter.failed[0].errorCode, "no_payload")
	}
}

func TestHandleOrderRequest_UnknownType(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "uuid-5",
		OrderType:    "bogus",
		PayloadCode:  "PART-A",
		DeliveryNode: lineNode.Name,
	})

	if len(emitter.failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(emitter.failed))
	}
	if emitter.failed[0].errorCode != "unknown_type" {
		t.Errorf("error code = %q, want %q", emitter.failed[0].errorCode, "unknown_type")
	}
}

func TestHandleOrderRequest_UsesRegisteredPlanner(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)

	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())
	d.RegisterPlanner("custom_transfer", func(order *orders.Order, env *protocol.Envelope, payloadCode string) (*PlanningResult, *planningError) {
		if err := db.UpdateOrderSourceNode(order.ID, storageNode.Name); err != nil {
			t.Fatalf("update source node: %v", err)
		}
		order.SourceNode = storageNode.Name
		return &PlanningResult{
			SourceNode: storageNode,
			DestNode:   lineNode,
		}, nil
	})

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "uuid-custom",
		OrderType:    "custom_transfer",
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})

	if len(emitter.received) != 1 {
		t.Fatalf("received events = %d, want 1", len(emitter.received))
	}
	if len(emitter.failed) != 1 {
		t.Fatalf("failed events = %d, want 1 because mock backend refuses dispatch", len(emitter.failed))
	}
	if emitter.failed[0].errorCode != "fleet_failed" {
		t.Fatalf("error code = %q, want fleet_failed", emitter.failed[0].errorCode)
	}
}

func TestHandleOrderRequest_UnknownStyle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:    "uuid-pt",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  "NONEXISTENT",
		DeliveryNode: lineNode.Name,
	})

	// Should fail before creating order — no received or failed events from emitter
	// but an error reply should be enqueued in the outbox
}

func TestHandleOrderCancel(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	order := &orders.Order{EdgeUUID: "uuid-cancel", StationID: "line-1", Status: StatusPending}
	db.CreateOrder(order)

	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	env := testEnvelope()
	d.HandleOrderCancel(env, &protocol.OrderCancel{OrderUUID: "uuid-cancel", Reason: "operator cancelled"})

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
	t.Parallel()
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)

	order := &orders.Order{EdgeUUID: "uuid-unclaim", StationID: "line-1", Status: StatusDispatched}
	db.CreateOrder(order)

	// Create a bin at the storage node
	bin := &bins.Bin{BinTypeID: 1, Label: "BIN-UC-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin)

	db.SetBinManifest(bin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")
	db.ClaimBin(bin.ID, order.ID)

	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())

	env := testEnvelope()
	d.HandleOrderCancel(env, &protocol.OrderCancel{OrderUUID: "uuid-unclaim", Reason: "test"})

	// Verify bin unclaimed
	got, _ := db.GetBin(bin.ID)
	if got.ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v, want nil", got.ClaimedBy)
	}
}

func TestHandleOrderReceipt(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	order := &orders.Order{EdgeUUID: "uuid-receipt", StationID: "line-1", Status: StatusDelivered}
	db.CreateOrder(order)

	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	env := testEnvelope()
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{OrderUUID: "uuid-receipt", ReceiptType: "confirmed", FinalCount: 50})

	if len(emitter.completed) != 1 {
		t.Fatalf("completed events = %d, want 1", len(emitter.completed))
	}

	// Verify order is completed
	got, _ := db.GetOrder(order.ID)
	if got.Status != StatusConfirmed {
		t.Errorf("status = %q, want %q", got.Status, StatusConfirmed)
	}
}

func TestHandleOrderCancel_RejectsWrongStation(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	order := &orders.Order{EdgeUUID: "uuid-owned", StationID: "line-1", Status: StatusPending}
	db.CreateOrder(order)

	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	// Attempt cancel from a different station
	env := &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-2"},
		Dst: protocol.Address{Role: protocol.RoleCore},
	}
	d.HandleOrderCancel(env, &protocol.OrderCancel{OrderUUID: "uuid-owned", Reason: "hijack"})

	if len(emitter.cancelled) != 0 {
		t.Fatalf("cancelled events = %d, want 0 (wrong station should be rejected)", len(emitter.cancelled))
	}

	// Verify order still pending
	got, _ := db.GetOrder(order.ID)
	if got.Status != StatusPending {
		t.Errorf("status = %q, want %q (order should be unchanged)", got.Status, StatusPending)
	}
}

func TestHandleOrderCancel_AllowsCoreRole(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	order := &orders.Order{EdgeUUID: "uuid-core-cancel", StationID: "line-1", Status: StatusPending}
	db.CreateOrder(order)

	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	// Core-role sender should bypass ownership check
	env := &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleCore, Station: "core-test"},
		Dst: protocol.Address{Role: protocol.RoleCore},
	}
	d.HandleOrderCancel(env, &protocol.OrderCancel{OrderUUID: "uuid-core-cancel", Reason: "admin cancel"})

	if len(emitter.cancelled) != 1 {
		t.Fatalf("cancelled events = %d, want 1 (core role should be allowed)", len(emitter.cancelled))
	}

	got, _ := db.GetOrder(order.ID)
	if got.Status != StatusCancelled {
		t.Errorf("status = %q, want %q", got.Status, StatusCancelled)
	}
}

func TestHandleOrderCancel_DuplicateCancelledOrderIgnored(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	order := &orders.Order{EdgeUUID: "uuid-cancel-dupe", StationID: "edge-1", Status: StatusCancelled}
	db.CreateOrder(order)

	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	env := &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "edge-1"},
		Dst: protocol.Address{Role: protocol.RoleCore},
	}
	d.HandleOrderCancel(env, &protocol.OrderCancel{OrderUUID: "uuid-cancel-dupe", Reason: "duplicate"})

	if len(emitter.cancelled) != 0 {
		t.Fatalf("cancelled events = %d, want 0 (duplicate cancel should be ignored)", len(emitter.cancelled))
	}

	msgs, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("outbox messages = %d, want 0 (duplicate cancel should not enqueue replies)", len(msgs))
	}
}

func TestHandleOrderReceipt_RejectsWrongStation(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	order := &orders.Order{EdgeUUID: "uuid-receipt-own", StationID: "line-1", Status: StatusDelivered}
	db.CreateOrder(order)

	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	env := &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-2"},
		Dst: protocol.Address{Role: protocol.RoleCore},
	}
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{OrderUUID: "uuid-receipt-own", ReceiptType: "confirmed", FinalCount: 10})

	if len(emitter.completed) != 0 {
		t.Fatalf("completed events = %d, want 0 (wrong station should be rejected)", len(emitter.completed))
	}

	got, _ := db.GetOrder(order.ID)
	if got.Status != StatusDelivered {
		t.Errorf("status = %q, want %q (order should be unchanged)", got.Status, StatusDelivered)
	}
}

func TestHandleOrderRedirect_RejectsWrongStation(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	order := &orders.Order{EdgeUUID: "uuid-redir-own", StationID: "line-1", Status: StatusDispatched, SourceNode: lineNode.Name}
	db.CreateOrder(order)

	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())

	env := &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-2"},
		Dst: protocol.Address{Role: protocol.RoleCore},
	}
	d.HandleOrderRedirect(env, &protocol.OrderRedirect{OrderUUID: "uuid-redir-own", NewDeliveryNode: lineNode.Name})

	got, _ := db.GetOrder(order.ID)
	if got.Status != StatusDispatched {
		t.Errorf("status = %q, want %q (order should be unchanged)", got.Status, StatusDispatched)
	}
}

func TestFIFOPayloadSourceSelection(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)

	// Create another storage node
	s2 := &nodes.Node{Name: "STORAGE-B1", Enabled: true}
	db.CreateNode(s2)

	// Create bins at each storage node
	bin1 := &bins.Bin{BinTypeID: 1, Label: "BIN-FIFO-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin1)
	bin2 := &bins.Bin{BinTypeID: 1, Label: "BIN-FIFO-2", NodeID: &s2.ID, Status: "available"}
	db.CreateBin(bin2)

	// Older available bin at storageNode
	db.SetBinManifest(bin1.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin1.ID, "")
	// Newer available bin at s2
	db.SetBinManifest(bin2.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin2.ID, "")

	// FIFO should select oldest (bin1) first
	source, err := db.FindSourceBinFIFO("PART-A", 0)
	if err != nil {
		t.Fatalf("FindSourceBinFIFO: %v", err)
	}
	if source.ID != bin1.ID {
		t.Errorf("source bin = %d, want %d (FIFO order)", source.ID, bin1.ID)
	}
}

func TestStatusConstants(t *testing.T) {
	t.Parallel()
	// Verify all plan-defined statuses exist
	statuses := []protocol.Status{
		StatusPending, StatusSourcing, StatusSubmitted, StatusDispatched,
		StatusAcknowledged, StatusInTransit, StatusDelivered, StatusConfirmed,
		StatusFailed, StatusCancelled,
	}
	expected := []string{
		"pending", "sourcing", "submitted", "dispatched",
		"acknowledged", "in_transit", "delivered", "confirmed",
		"failed", "cancelled",
	}
	for i, s := range statuses {
		if string(s) != expected[i] {
			t.Errorf("status[%d] = %q, want %q", i, s, expected[i])
		}
	}
}

func TestOrderTypeConstants(t *testing.T) {
	t.Parallel()
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

// --- Regression: HandleOrderReceipt returns on ConfirmReceipt error ---
// Before fix, ConfirmReceipt errors were logged but execution continued,
// leaving the order in a partially processed state. Now it returns early.
// This test sends a receipt for an order NOT in "delivered" status, which
// causes ConfirmReceipt to fail. Verifies the order status is unchanged
// (the return prevented any further processing).
func TestRegression_HandleOrderReceipt_ReturnsOnError(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	// Create order in "dispatched" status (NOT delivered).
	// ConfirmReceipt requires status == delivered, so this will fail.
	order := &orders.Order{
		EdgeUUID:     "receipt-err-1",
		StationID:    "edge.line1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusDispatched,
		Quantity:     1,
		DeliveryNode: lineNode.Name,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}

	env := testEnvelope()
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{
		OrderUUID:   "receipt-err-1",
		ReceiptType: "confirmed",
		FinalCount:  1,
	})

	// Verify: order status unchanged — still dispatched
	got, err := db.GetOrderByUUID("receipt-err-1")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if got.Status != StatusDispatched {
		t.Errorf("order status = %q after failed receipt, want %q (should not have changed)", got.Status, StatusDispatched)
	}

	// Verify: no completion event emitted
	if len(emitter.completed) > 0 {
		t.Errorf("completed events = %d, want 0 (receipt failed, should not complete)", len(emitter.completed))
	}
}
