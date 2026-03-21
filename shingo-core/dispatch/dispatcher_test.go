package dispatch

import (
	"context"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingocore/config"
	"shingocore/fleet"
	"shingocore/store"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
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
	orderID   int64
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

func (m *mockEmitter) EmitOrderReceived(orderID int64, _, _, _, payloadCode, _ string) {
	m.received = append(m.received, emitReceived{orderID, payloadCode})
}
func (m *mockEmitter) EmitOrderDispatched(orderID int64, vendorOrderID, _, _ string) {
	m.dispatched = append(m.dispatched, emitDispatched{orderID, vendorOrderID})
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

// --- Mock fleet backend ---

type mockBackend struct{}

func (m *mockBackend) CreateTransportOrder(req fleet.TransportOrderRequest) (fleet.TransportOrderResult, error) {
	return fleet.TransportOrderResult{}, fmt.Errorf("mock: not connected")
}
func (m *mockBackend) CancelOrder(vendorOrderID string) error {
	return fmt.Errorf("mock: not connected")
}
func (m *mockBackend) SetOrderPriority(vendorOrderID string, priority int) error {
	return fmt.Errorf("mock: not connected")
}
func (m *mockBackend) Ping() error                    { return fmt.Errorf("mock: not connected") }
func (m *mockBackend) Name() string                   { return "mock" }
func (m *mockBackend) MapState(vendorState string) string { return "dispatched" }
func (m *mockBackend) IsTerminalState(vendorState string) bool { return false }
func (m *mockBackend) Reconfigure(cfg fleet.ReconfigureParams) {}
func (m *mockBackend) CreateStagedOrder(req fleet.StagedOrderRequest) (fleet.TransportOrderResult, error) {
	return fleet.TransportOrderResult{}, fmt.Errorf("mock: not connected")
}
func (m *mockBackend) ReleaseOrder(vendorOrderID string, blocks []fleet.OrderBlock) error {
	return fmt.Errorf("mock: not connected")
}

// --- Test helpers ---

func testDB(t *testing.T) *store.DB {
	t.Helper()
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("shingocore_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp")),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { pgContainer.Terminate(ctx) })

	host, _ := pgContainer.Host(ctx)
	port, _ := pgContainer.MappedPort(ctx, "5432")

	db, err := store.Open(&config.DatabaseConfig{
		Postgres: config.PostgresConfig{
			Host:     host,
			Port:     port.Int(),
			Database: "shingocore_test",
			User:     "test",
			Password: "test",
			SSLMode:  "disable",
		},
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func setupTestData(t *testing.T, db *store.DB) (storageNode *store.Node, lineNode *store.Node, bp *store.Payload) {
	t.Helper()
	storageNode = &store.Node{Name: "STORAGE-A1", Zone: "A", Enabled: true}
	if err := db.CreateNode(storageNode); err != nil {
		t.Fatalf("create storage node: %v", err)
	}
	lineNode = &store.Node{Name: "LINE1-IN", Enabled: true}
	if err := db.CreateNode(lineNode); err != nil {
		t.Fatalf("create line node: %v", err)
	}
	bp = &store.Payload{Code: "PART-A", Description: "Steel bracket tote"}
	if err := db.CreatePayload(bp); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	// Create a default bin type used by test bins
	bt := &store.BinType{Code: "DEFAULT", Description: "Default test bin type"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	return
}

func newTestDispatcher(t *testing.T, db *store.DB, backend fleet.Backend) (*Dispatcher, *mockEmitter) {
	t.Helper()
	emitter := &mockEmitter{}
	d := NewDispatcher(db, backend, emitter, "core", "shingo.dispatch", nil)
	return d, emitter
}

func testEnvelope() *protocol.Envelope {
	return &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-1"},
		Dst: protocol.Address{Role: protocol.RoleCore},
	}
}

// --- Tests ---

func TestHandleOrderRequest_Retrieve_NoSource(t *testing.T) {
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	// No fleet backend needed since it should fail before dispatch
	d, emitter := newTestDispatcher(t, db, &mockBackend{})

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "uuid-1",
		OrderType:       OrderTypeRetrieve,
		PayloadCode: "PART-A",
		DeliveryNode:    lineNode.Name,
		Quantity:        1.0,
	})

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

	d, emitter := newTestDispatcher(t, db, &mockBackend{})

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "uuid-2",
		OrderType:       OrderTypeRetrieve,
		PayloadCode: "PART-A",
		DeliveryNode:    "NONEXISTENT",
		Quantity:        1.0,
	})

	// Should get an error reply enqueued (delivery node not found)
	if len(emitter.received) != 0 {
		t.Errorf("received events = %d, want 0 (should fail before order creation)", len(emitter.received))
	}
}

func TestHandleOrderRequest_Move_MissingPickup(t *testing.T) {
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	d, emitter := newTestDispatcher(t, db, &mockBackend{})

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "uuid-3",
		OrderType:       OrderTypeMove,
		PayloadCode: "PART-A",
		DeliveryNode:    lineNode.Name,
		PickupNode:      "",
		Quantity:        1.0,
	})

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

	d, emitter := newTestDispatcher(t, db, &mockBackend{})

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "uuid-4",
		OrderType:       OrderTypeMove,
		PayloadCode: "PART-A",
		DeliveryNode:    lineNode.Name,
		PickupNode:      storageNode.Name,
		Quantity:        1.0,
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
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	d, emitter := newTestDispatcher(t, db, &mockBackend{})

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "uuid-5",
		OrderType:       "bogus",
		PayloadCode: "PART-A",
		DeliveryNode:    lineNode.Name,
	})

	if len(emitter.failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(emitter.failed))
	}
	if emitter.failed[0].errorCode != "unknown_type" {
		t.Errorf("error code = %q, want %q", emitter.failed[0].errorCode, "unknown_type")
	}
}

func TestHandleOrderRequest_UnknownStyle(t *testing.T) {
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, &mockBackend{})

	env := testEnvelope()
	d.HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID:       "uuid-pt",
		OrderType:       OrderTypeRetrieve,
		PayloadCode: "NONEXISTENT",
		DeliveryNode:    lineNode.Name,
	})

	// Should fail before creating order — no received or failed events from emitter
	// but an error reply should be enqueued in the outbox
}

func TestHandleOrderCancel(t *testing.T) {
	db := testDB(t)

	order := &store.Order{EdgeUUID: "uuid-cancel", StationID: "line-1", Status: StatusPending}
	db.CreateOrder(order)

	d, emitter := newTestDispatcher(t, db, &mockBackend{})

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
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)

	order := &store.Order{EdgeUUID: "uuid-unclaim", StationID: "line-1", Status: StatusDispatched}
	db.CreateOrder(order)

	// Create a bin at the storage node
	bin := &store.Bin{BinTypeID: 1, Label: "BIN-UC-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin)

	db.SetBinManifest(bin.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID)
	db.ClaimBin(bin.ID, order.ID)

	d, _ := newTestDispatcher(t, db, &mockBackend{})

	env := testEnvelope()
	d.HandleOrderCancel(env, &protocol.OrderCancel{OrderUUID: "uuid-unclaim", Reason: "test"})

	// Verify bin unclaimed
	got, _ := db.GetBin(bin.ID)
	if got.ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v, want nil", got.ClaimedBy)
	}
}

func TestHandleOrderReceipt(t *testing.T) {
	db := testDB(t)

	order := &store.Order{EdgeUUID: "uuid-receipt", StationID: "line-1", Status: StatusDelivered}
	db.CreateOrder(order)

	d, emitter := newTestDispatcher(t, db, &mockBackend{})

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
	db := testDB(t)

	order := &store.Order{EdgeUUID: "uuid-owned", StationID: "line-1", Status: StatusPending}
	db.CreateOrder(order)

	d, emitter := newTestDispatcher(t, db, &mockBackend{})

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
	db := testDB(t)

	order := &store.Order{EdgeUUID: "uuid-core-cancel", StationID: "line-1", Status: StatusPending}
	db.CreateOrder(order)

	d, emitter := newTestDispatcher(t, db, &mockBackend{})

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

func TestHandleOrderReceipt_RejectsWrongStation(t *testing.T) {
	db := testDB(t)

	order := &store.Order{EdgeUUID: "uuid-receipt-own", StationID: "line-1", Status: StatusDelivered}
	db.CreateOrder(order)

	d, emitter := newTestDispatcher(t, db, &mockBackend{})

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
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	order := &store.Order{EdgeUUID: "uuid-redir-own", StationID: "line-1", Status: StatusDispatched, PickupNode: lineNode.Name}
	db.CreateOrder(order)

	d, _ := newTestDispatcher(t, db, &mockBackend{})

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
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)

	// Create another storage node
	s2 := &store.Node{Name: "STORAGE-B1", Enabled: true}
	db.CreateNode(s2)

	// Create bins at each storage node
	bin1 := &store.Bin{BinTypeID: 1, Label: "BIN-FIFO-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin1)
	bin2 := &store.Bin{BinTypeID: 1, Label: "BIN-FIFO-2", NodeID: &s2.ID, Status: "available"}
	db.CreateBin(bin2)

	// Older available bin at storageNode
	db.SetBinManifest(bin1.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin1.ID)
	// Newer available bin at s2
	db.SetBinManifest(bin2.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin2.ID)

	// FIFO should select oldest (bin1) first
	source, err := db.FindSourceBinFIFO("PART-A")
	if err != nil {
		t.Fatalf("FindSourceBinFIFO: %v", err)
	}
	if source.ID != bin1.ID {
		t.Errorf("source bin = %d, want %d (FIFO order)", source.ID, bin1.ID)
	}
}

func TestStatusConstants(t *testing.T) {
	// Verify all plan-defined statuses exist
	statuses := []string{
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
