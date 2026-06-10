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

// captureSetOrder stands up a fake RDS that records the SetOrderRequest body
// posted to /setOrder, so a test can assert what actually went on the wire.
func captureSetOrder(t *testing.T) (*httptest.Server, func() rds.SetOrderRequest) {
	t.Helper()
	var mu sync.Mutex
	var got rds.SetOrderRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/setOrder" {
			mu.Lock()
			json.NewDecoder(r.Body).Decode(&got)
			mu.Unlock()
		}
		w.Write([]byte(`{"code":0,"msg":"ok"}`))
	}))
	return srv, func() rds.SetOrderRequest {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

// TestCreateTransportOrder_StampsRobotGroup pins the F1 wire contract: the
// RobotGroup on the transport request must land in rds.SetOrderRequest.Group
// (SEER's robot-dispatch group). Empty → omitted, so SEER keeps its default
// robot assignment (backward-compatible).
func TestCreateTransportOrder_StampsRobotGroup(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, group, want string }{
		{"explicit group routes to that robot class", "heavy-1500", "heavy-1500"},
		{"empty group → vendor default", "", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, captured := captureSetOrder(t)
			defer srv.Close()

			adapter := New(Config{BaseURL: srv.URL, Timeout: 5 * time.Second, DebugLog: func(string, ...any) {}})
			if _, err := adapter.CreateTransportOrder(fleet.TransportOrderRequest{
				OrderID:    "sg-1-aaa",
				ExternalID: "uuid-1",
				FromLoc:    "LINE-01",
				ToLoc:      "SMN_001",
				RobotGroup: tc.group,
			}); err != nil {
				t.Fatalf("CreateTransportOrder: %v", err)
			}
			if g := captured().Group; g != tc.want {
				t.Errorf("SetOrderRequest.Group = %q, want %q", g, tc.want)
			}
		})
	}
}

// TestCreateStagedOrder_StampsRobotGroup is the same contract on the staged
// (complex / swap) dispatch path.
func TestCreateStagedOrder_StampsRobotGroup(t *testing.T) {
	t.Parallel()
	srv, captured := captureSetOrder(t)
	defer srv.Close()

	adapter := New(Config{BaseURL: srv.URL, Timeout: 5 * time.Second, DebugLog: func(string, ...any) {}})
	if _, err := adapter.CreateStagedOrder(fleet.StagedOrderRequest{
		OrderID:    "sg-2-bbb",
		ExternalID: "uuid-2",
		Blocks:     []fleet.OrderBlock{{BlockID: "b1", Location: "LINE-01", BinTask: "JackLoad"}},
		RobotGroup: "heavy-1500",
	}); err != nil {
		t.Fatalf("CreateStagedOrder: %v", err)
	}
	if g := captured().Group; g != "heavy-1500" {
		t.Errorf("staged SetOrderRequest.Group = %q, want heavy-1500", g)
	}
}
