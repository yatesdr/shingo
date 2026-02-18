package rds

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func testServer(handler http.HandlerFunc) (*httptest.Server, *Client) {
	srv := httptest.NewServer(handler)
	client := NewClient(srv.URL, 5*time.Second)
	return srv, client
}

func TestCreateJoinOrder(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/setOrder" {
			t.Errorf("path = %q, want /setOrder", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}

		var req SetJoinOrderRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.ID != "wp-1-abc" {
			t.Errorf("ID = %q, want %q", req.ID, "wp-1-abc")
		}
		if req.FromLoc != "Loc-A" {
			t.Errorf("FromLoc = %q, want %q", req.FromLoc, "Loc-A")
		}
		if req.ToLoc != "Loc-B" {
			t.Errorf("ToLoc = %q, want %q", req.ToLoc, "Loc-B")
		}

		json.NewEncoder(w).Encode(Response{Code: 0, Msg: "ok"})
	})
	defer srv.Close()

	err := client.CreateJoinOrder(&SetJoinOrderRequest{
		ID:      "wp-1-abc",
		FromLoc: "Loc-A",
		ToLoc:   "Loc-B",
	})
	if err != nil {
		t.Fatalf("CreateJoinOrder: %v", err)
	}
}

func TestCreateJoinOrder_Error(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Code: 1, Msg: "order already exists"})
	})
	defer srv.Close()

	err := client.CreateJoinOrder(&SetJoinOrderRequest{ID: "dup"})
	if err == nil {
		t.Fatal("expected error for non-zero response code")
	}
}

func TestGetOrderDetails(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orderDetails/order-123" {
			t.Errorf("path = %q, want /orderDetails/order-123", r.URL.Path)
		}
		json.NewEncoder(w).Encode(OrderDetailsResponse{
			Response: Response{Code: 0},
			Data: &OrderDetail{
				ID:      "order-123",
				State:   StateRunning,
				Vehicle: "AMB-01",
				FromLoc: "Loc-A",
				ToLoc:   "Loc-B",
			},
		})
	})
	defer srv.Close()

	detail, err := client.GetOrderDetails("order-123")
	if err != nil {
		t.Fatalf("GetOrderDetails: %v", err)
	}
	if detail.ID != "order-123" {
		t.Errorf("ID = %q, want %q", detail.ID, "order-123")
	}
	if detail.State != StateRunning {
		t.Errorf("State = %q, want %q", detail.State, StateRunning)
	}
	if detail.Vehicle != "AMB-01" {
		t.Errorf("Vehicle = %q, want %q", detail.Vehicle, "AMB-01")
	}
}

func TestGetOrderDetails_NotFound(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(OrderDetailsResponse{
			Response: Response{Code: 1, Msg: "not found"},
		})
	})
	defer srv.Close()

	_, err := client.GetOrderDetails("missing")
	if err == nil {
		t.Fatal("expected error for not found")
	}
}

func TestTerminateOrder(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/terminate" {
			t.Errorf("path = %q, want /terminate", r.URL.Path)
		}
		var req TerminateRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.ID != "order-456" {
			t.Errorf("ID = %q, want %q", req.ID, "order-456")
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	err := client.TerminateOrder(&TerminateRequest{ID: "order-456"})
	if err != nil {
		t.Fatalf("TerminateOrder: %v", err)
	}
}

func TestListOrders(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orders" {
			t.Errorf("path = %q, want /orders", r.URL.Path)
		}
		page := r.URL.Query().Get("page")
		size := r.URL.Query().Get("size")
		if page != "1" || size != "10" {
			t.Errorf("page=%s size=%s, want page=1 size=10", page, size)
		}
		json.NewEncoder(w).Encode(OrderListResponse{
			Response: Response{Code: 0},
			Data: []OrderDetail{
				{ID: "o1", State: StateRunning},
				{ID: "o2", State: StateFinished},
			},
		})
	})
	defer srv.Close()

	orders, err := client.ListOrders(1, 10)
	if err != nil {
		t.Fatalf("ListOrders: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("len = %d, want 2", len(orders))
	}
	if orders[0].ID != "o1" {
		t.Errorf("orders[0].ID = %q, want %q", orders[0].ID, "o1")
	}
}

func TestSetPriority(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/setPriority" {
			t.Errorf("path = %q, want /setPriority", r.URL.Path)
		}
		var req SetPriorityRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.ID != "order-789" {
			t.Errorf("ID = %q, want %q", req.ID, "order-789")
		}
		if req.Priority != 10 {
			t.Errorf("Priority = %d, want 10", req.Priority)
		}
		json.NewEncoder(w).Encode(Response{Code: 0})
	})
	defer srv.Close()

	err := client.SetPriority("order-789", 10)
	if err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
}

func TestPing(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ping" {
			t.Errorf("path = %q, want /ping", r.URL.Path)
		}
		json.NewEncoder(w).Encode(PingResponse{Product: "RDS Core", Version: "3.0"})
	})
	defer srv.Close()

	resp, err := client.Ping()
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if resp.Product != "RDS Core" {
		t.Errorf("Product = %q, want %q", resp.Product, "RDS Core")
	}
}

func TestGetRobotsStatus(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robotsStatus" {
			t.Errorf("path = %q, want /robotsStatus", r.URL.Path)
		}
		json.NewEncoder(w).Encode(RobotsStatusResponse{
			Response: Response{Code: 0},
			Report: []RobotStatus{
				{VehicleID: "AMB-01", Dispatchable: true},
			},
		})
	})
	defer srv.Close()

	robots, err := client.GetRobotsStatus()
	if err != nil {
		t.Fatalf("GetRobotsStatus: %v", err)
	}
	if len(robots) != 1 {
		t.Fatalf("len = %d, want 1", len(robots))
	}
	if robots[0].VehicleID != "AMB-01" {
		t.Errorf("VehicleID = %q, want %q", robots[0].VehicleID, "AMB-01")
	}
}

func TestHTTPError(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	})
	defer srv.Close()

	err := client.CreateJoinOrder(&SetJoinOrderRequest{ID: "fail"})
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestCheckResponse(t *testing.T) {
	if err := checkResponse(&Response{Code: 0, Msg: "ok"}); err != nil {
		t.Errorf("code 0 should not error: %v", err)
	}
	if err := checkResponse(&Response{Code: 1, Msg: "fail"}); err == nil {
		t.Error("code 1 should error")
	}
}

func TestOrderStateIsTerminal(t *testing.T) {
	tests := []struct {
		state    OrderState
		terminal bool
	}{
		{StateCreated, false},
		{StateToBeDispatched, false},
		{StateRunning, false},
		{StateWaiting, false},
		{StateFinished, true},
		{StateFailed, true},
		{StateStopped, true},
	}
	for _, tt := range tests {
		if got := tt.state.IsTerminal(); got != tt.terminal {
			t.Errorf("IsTerminal(%q) = %v, want %v", tt.state, got, tt.terminal)
		}
	}
}

// --- Poller tests ---

type mockPollerEmitter struct {
	mu     sync.Mutex
	events []pollerEvent
}

type pollerEvent struct {
	orderID    int64
	rdsOrderID string
	oldStatus  string
	newStatus  string
	robotID    string
}

func (m *mockPollerEmitter) EmitOrderStatusChanged(orderID int64, rdsOrderID, oldStatus, newStatus, robotID, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, pollerEvent{orderID, rdsOrderID, oldStatus, newStatus, robotID})
}

type mockResolver struct{}

func (m *mockResolver) ResolveRDSOrderID(rdsOrderID string) (int64, error) {
	return 100, nil
}

func TestPollerTrackUntrack(t *testing.T) {
	client := NewClient("http://localhost:9999", time.Second)
	emitter := &mockPollerEmitter{}
	resolver := &mockResolver{}
	p := NewPoller(client, emitter, resolver, time.Minute)

	if p.ActiveCount() != 0 {
		t.Errorf("initial count = %d, want 0", p.ActiveCount())
	}

	p.Track("order-1")
	p.Track("order-2")
	if p.ActiveCount() != 2 {
		t.Errorf("count after track = %d, want 2", p.ActiveCount())
	}

	// Track duplicate is idempotent
	p.Track("order-1")
	if p.ActiveCount() != 2 {
		t.Errorf("count after dup track = %d, want 2", p.ActiveCount())
	}

	p.Untrack("order-1")
	if p.ActiveCount() != 1 {
		t.Errorf("count after untrack = %d, want 1", p.ActiveCount())
	}

	p.Untrack("nonexistent")
	if p.ActiveCount() != 1 {
		t.Errorf("count after untrack nonexistent = %d, want 1", p.ActiveCount())
	}
}

func TestPollerDetectsStateTransition(t *testing.T) {
	callCount := 0
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		state := StateCreated
		if callCount > 1 {
			state = StateRunning
		}
		json.NewEncoder(w).Encode(OrderDetailsResponse{
			Response: Response{Code: 0},
			Data: &OrderDetail{
				ID:      "rds-1",
				State:   state,
				Vehicle: "AMB-01",
			},
		})
	})
	defer srv.Close()

	emitter := &mockPollerEmitter{}
	resolver := &mockResolver{}
	p := NewPoller(client, emitter, resolver, time.Minute)

	p.Track("rds-1")

	// First poll establishes baseline (CREATED -> CREATED, no change since Track sets initial to CREATED)
	p.poll()
	if len(emitter.events) != 0 {
		t.Errorf("events after first poll = %d, want 0", len(emitter.events))
	}

	// Second poll detects CREATED -> RUNNING
	p.poll()
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if len(emitter.events) != 1 {
		t.Fatalf("events after second poll = %d, want 1", len(emitter.events))
	}
	ev := emitter.events[0]
	if ev.oldStatus != "CREATED" {
		t.Errorf("oldStatus = %q, want %q", ev.oldStatus, "CREATED")
	}
	if ev.newStatus != "RUNNING" {
		t.Errorf("newStatus = %q, want %q", ev.newStatus, "RUNNING")
	}
	if ev.robotID != "AMB-01" {
		t.Errorf("robotID = %q, want %q", ev.robotID, "AMB-01")
	}
}

func TestPollerRemovesTerminal(t *testing.T) {
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(OrderDetailsResponse{
			Response: Response{Code: 0},
			Data:     &OrderDetail{ID: "rds-2", State: StateFinished},
		})
	})
	defer srv.Close()

	emitter := &mockPollerEmitter{}
	resolver := &mockResolver{}
	p := NewPoller(client, emitter, resolver, time.Minute)

	p.Track("rds-2")
	p.poll()

	if p.ActiveCount() != 0 {
		t.Errorf("count after terminal = %d, want 0", p.ActiveCount())
	}
}
