package seerrds

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"shingocore/fleet"
	"shingocore/rds"
)

// TestReleaseOrder_PinsVehicle verifies that when releasing a staged order,
// the adapter queries the order details to find the assigned vehicle and
// includes it in the addBlocks request so RDS keeps the same robot.
func TestReleaseOrder_PinsVehicle(t *testing.T) {
	var mu sync.Mutex
	var addBlocksReq rds.AddBlocksRequest
	var addBlocksCalled bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orderDetails/sg-42-abc":
			// Simulate RDS returning the vehicle assigned to the staged order
			w.Write([]byte(`{"code":0,"msg":"ok","id":"sg-42-abc","state":"WAITING","vehicle":"AMB-03"}`))

		case "/addBlocks":
			mu.Lock()
			json.NewDecoder(r.Body).Decode(&addBlocksReq)
			addBlocksCalled = true
			mu.Unlock()
			w.Write([]byte(`{"code":0,"msg":"ok"}`))

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := New(Config{
		BaseURL:  srv.URL,
		Timeout:  5 * time.Second,
		DebugLog: func(string, ...any) {},
	})

	err := adapter.ReleaseOrder("sg-42-abc", []fleet.OrderBlock{
		{BlockID: "sg-42-abc-b3", Location: "LINE-01", BinTask: "JackLoad"},
		{BlockID: "sg-42-abc-b4", Location: "OUTBOUND-STG", BinTask: "JackUnload"},
	}, true)
	if err != nil {
		t.Fatalf("ReleaseOrder: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if !addBlocksCalled {
		t.Fatal("addBlocks was never called")
	}
	if addBlocksReq.Vehicle != "AMB-03" {
		t.Errorf("addBlocks vehicle = %q, want %q (should pin the staged order's robot)", addBlocksReq.Vehicle, "AMB-03")
	}
	if !addBlocksReq.Complete {
		t.Error("addBlocks complete = false, want true")
	}
	if len(addBlocksReq.Blocks) != 2 {
		t.Fatalf("addBlocks blocks = %d, want 2", len(addBlocksReq.Blocks))
	}
}

// TestReleaseOrder_NoVehicleFallback verifies that if the order details
// lookup fails (e.g., order not found), the release still succeeds
// without pinning a vehicle — RDS picks freely.
func TestReleaseOrder_NoVehicleFallback(t *testing.T) {
	var mu sync.Mutex
	var addBlocksReq rds.AddBlocksRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orderDetails/sg-99-missing":
			// Simulate RDS returning error for unknown order
			w.Write([]byte(`{"code":1,"msg":"not found"}`))

		case "/addBlocks":
			mu.Lock()
			json.NewDecoder(r.Body).Decode(&addBlocksReq)
			mu.Unlock()
			w.Write([]byte(`{"code":0,"msg":"ok"}`))
		}
	}))
	defer srv.Close()

	adapter := New(Config{
		BaseURL:  srv.URL,
		Timeout:  5 * time.Second,
		DebugLog: func(string, ...any) {},
	})

	err := adapter.ReleaseOrder("sg-99-missing", []fleet.OrderBlock{
		{BlockID: "sg-99-missing-b2", Location: "DEST", BinTask: "JackUnload"},
	}, true)
	if err != nil {
		t.Fatalf("ReleaseOrder: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if addBlocksReq.Vehicle != "" {
		t.Errorf("addBlocks vehicle = %q, want empty (no vehicle to pin)", addBlocksReq.Vehicle)
	}
}
