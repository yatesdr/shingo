package messaging

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/store"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

type countingBackend struct {
	cancels int
}

func (m *countingBackend) CreateTransportOrder(req fleet.TransportOrderRequest) (fleet.TransportOrderResult, error) {
	return fleet.TransportOrderResult{}, fmt.Errorf("mock: not connected")
}
func (m *countingBackend) CancelOrder(vendorOrderID string) error {
	m.cancels++
	return nil
}
func (m *countingBackend) SetOrderPriority(vendorOrderID string, priority int) error {
	return nil
}
func (m *countingBackend) Ping() error                             { return nil }
func (m *countingBackend) Name() string                            { return "counting" }
func (m *countingBackend) MapState(vendorState string) string      { return vendorState }
func (m *countingBackend) IsTerminalState(vendorState string) bool { return false }
func (m *countingBackend) Reconfigure(cfg fleet.ReconfigureParams) {}
func (m *countingBackend) CreateStagedOrder(req fleet.StagedOrderRequest) (fleet.TransportOrderResult, error) {
	return fleet.TransportOrderResult{}, fmt.Errorf("mock: not connected")
}
func (m *countingBackend) ReleaseOrder(vendorOrderID string, blocks []fleet.OrderBlock) error {
	return nil
}

type noopEmitter struct{}

func (noopEmitter) EmitOrderReceived(orderID int64, edgeUUID, stationID, orderType, payloadCode, deliveryNode string) {
}
func (noopEmitter) EmitOrderDispatched(orderID int64, vendorOrderID, sourceNode, destNode string) {}
func (noopEmitter) EmitOrderFailed(orderID int64, edgeUUID, stationID, errorCode, detail string)  {}
func (noopEmitter) EmitOrderCancelled(orderID int64, edgeUUID, stationID, reason string)          {}
func (noopEmitter) EmitOrderCompleted(orderID int64, edgeUUID, stationID string)                  {}

func testDB(t *testing.T) *store.DB {
	t.Helper()
	ctx := context.Background()
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprint(r)
			if strings.Contains(strings.ToLower(msg), "docker") {
				t.Skipf("skipping integration test: %s", msg)
			}
			panic(r)
		}
	}()

	pgContainer, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("shingocore_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second)),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "docker") {
			t.Skipf("skipping integration test: %v", err)
		}
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { pgContainer.Terminate(ctx) })

	host, err := pgContainer.Host(ctx)
	if err != nil {
		t.Fatalf("get container host: %v", err)
	}
	port, err := pgContainer.MappedPort(ctx, "5432")
	if err != nil {
		t.Fatalf("get container port: %v", err)
	}

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

func TestCoreHandlerDeduplicatesRedirectByEnvelopeID(t *testing.T) {
	db := testDB(t)
	line := &store.Node{Name: "LINE-1", Enabled: true}
	dest := &store.Node{Name: "LINE-2", Enabled: true}
	if err := db.CreateNode(line); err != nil {
		t.Fatalf("create line node: %v", err)
	}
	if err := db.CreateNode(dest); err != nil {
		t.Fatalf("create destination node: %v", err)
	}

	order := &store.Order{
		EdgeUUID:      "uuid-redir",
		StationID:     "edge.1",
		OrderType:     dispatch.OrderTypeMove,
		Status:        dispatch.StatusDispatched,
		Quantity:      1,
		PickupNode:    line.Name,
		DeliveryNode:  line.Name,
		VendorOrderID: "vendor-1",
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderVendor(order.ID, "vendor-1", "CREATED", ""); err != nil {
		t.Fatalf("persist vendor order: %v", err)
	}
	backend := &countingBackend{}
	dispatcher := dispatch.NewDispatcher(db, backend, noopEmitter{}, "core", "dispatch", nil)
	handler := NewCoreHandler(db, nil, "core", "dispatch", dispatcher)

	env := &protocol.Envelope{
		ID:   "msg-redirect-1",
		Type: protocol.TypeOrderRedirect,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
		Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	req := &protocol.OrderRedirect{OrderUUID: order.EdgeUUID, NewDeliveryNode: dest.Name}

	handler.HandleOrderRedirect(env, req)
	handler.HandleOrderRedirect(env, req)

	if backend.cancels != 1 {
		t.Fatalf("expected redirect cancel to run once, got %d", backend.cancels)
	}
	got, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if got.DeliveryNode != dest.Name {
		t.Fatalf("expected delivery node %q, got %q", dest.Name, got.DeliveryNode)
	}
}

func TestCoreHandlerDeduplicatesOrderRequestByEnvelopeID(t *testing.T) {
	db := testDB(t)
	dest := &store.Node{Name: "LINE-REQ", Enabled: true}
	if err := db.CreateNode(dest); err != nil {
		t.Fatalf("create destination node: %v", err)
	}
	payload := &store.Payload{Code: "PART-A", Description: "Part A", UOPCapacity: 10}
	if err := db.CreatePayload(payload); err != nil {
		t.Fatalf("create payload: %v", err)
	}

	backend := &countingBackend{}
	dispatcher := dispatch.NewDispatcher(db, backend, noopEmitter{}, "core", "dispatch", nil)
	handler := NewCoreHandler(db, nil, "core", "dispatch", dispatcher)

	env := &protocol.Envelope{
		ID:   "msg-request-1",
		Type: protocol.TypeOrderRequest,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
		Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	req := &protocol.OrderRequest{
		OrderUUID:    "uuid-request-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  payload.Code,
		DeliveryNode: dest.Name,
		Quantity:     1,
	}

	handler.HandleOrderRequest(env, req)
	handler.HandleOrderRequest(env, req)

	orders, err := db.ListOrders("", 10)
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("expected 1 order after duplicate request replay, got %d", len(orders))
	}
}

func TestCoreHandlerDeduplicationPersistsAcrossHandlerRestart(t *testing.T) {
	db := testDB(t)
	dest := &store.Node{Name: "LINE-RESTART", Enabled: true}
	if err := db.CreateNode(dest); err != nil {
		t.Fatalf("create destination node: %v", err)
	}
	payload := &store.Payload{Code: "PART-R", Description: "Part R", UOPCapacity: 10}
	if err := db.CreatePayload(payload); err != nil {
		t.Fatalf("create payload: %v", err)
	}

	env := &protocol.Envelope{
		ID:   "msg-restart-1",
		Type: protocol.TypeOrderRequest,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
		Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	req := &protocol.OrderRequest{
		OrderUUID:    "uuid-request-restart",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  payload.Code,
		DeliveryNode: dest.Name,
		Quantity:     1,
	}

	first := NewCoreHandler(db, nil, "core", "dispatch", dispatch.NewDispatcher(db, &countingBackend{}, noopEmitter{}, "core", "dispatch", nil))
	first.HandleOrderRequest(env, req)

	second := NewCoreHandler(db, nil, "core", "dispatch", dispatch.NewDispatcher(db, &countingBackend{}, noopEmitter{}, "core", "dispatch", nil))
	second.HandleOrderRequest(env, req)

	orders, err := db.ListOrders("", 10)
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("expected 1 order after handler restart replay, got %d", len(orders))
	}
}

func TestCoreHandlerDeduplicatesReceiptAcrossHandlerRestart(t *testing.T) {
	db := testDB(t)
	order := &store.Order{
		EdgeUUID:     "uuid-receipt-restart",
		StationID:    "edge.1",
		OrderType:    dispatch.OrderTypeRetrieve,
		Status:       dispatch.StatusDelivered,
		DeliveryNode: "LINE-1",
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}

	env := &protocol.Envelope{
		ID:   "msg-receipt-restart-1",
		Type: protocol.TypeOrderReceipt,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
		Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	req := &protocol.OrderReceipt{OrderUUID: order.EdgeUUID, ReceiptType: "delivered", FinalCount: 1}

	first := NewCoreHandler(db, nil, "core", "dispatch", dispatch.NewDispatcher(db, &countingBackend{}, noopEmitter{}, "core", "dispatch", nil))
	first.HandleOrderReceipt(env, req)

	second := NewCoreHandler(db, nil, "core", "dispatch", dispatch.NewDispatcher(db, &countingBackend{}, noopEmitter{}, "core", "dispatch", nil))
	second.HandleOrderReceipt(env, req)

	got, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if got.CompletedAt == nil {
		t.Fatalf("expected completed_at to be set")
	}
	history, err := db.ListOrderHistory(order.ID)
	if err != nil {
		t.Fatalf("list order history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 history rows after replayed receipt, got %d", len(history))
	}
}
