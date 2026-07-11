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

// TestCreateOrder_StampsRobotGroup pins the robot-group wire contract on the
// unified create primitive: the RobotGroup on the request must land in
// rds.SetOrderRequest.Group (SEER's robot-dispatch group). Empty → omitted, so
// SEER keeps its default robot assignment (backward-compatible). Covers both
// lifecycles (Complete true/false) since they share one /setOrder call now.
func TestCreateOrder_StampsRobotGroup(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		group    string
		complete bool
		want     string
	}{
		{"explicit group routes to that robot class", "heavy-1500", true, "heavy-1500"},
		{"empty group → vendor default", "", true, ""},
		{"staged (Complete=false) also stamps the group", "heavy-1500", false, "heavy-1500"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, captured := captureSetOrder(t)
			defer srv.Close()

			adapter := New(Config{BaseURL: srv.URL, Timeout: 5 * time.Second, DebugLog: func(string, ...any) {}})
			if _, err := adapter.CreateOrder(fleet.CreateOrderRequest{
				OrderID:    "sg-1-aaa",
				ExternalID: "uuid-1",
				Blocks:     []fleet.OrderBlock{{BlockID: "b1", Location: "LINE-01", BinTask: "JackLoad"}},
				RobotGroup: tc.group,
				Complete:   tc.complete,
			}); err != nil {
				t.Fatalf("CreateOrder: %v", err)
			}
			got := captured()
			if g := got.Group; g != tc.want {
				t.Errorf("SetOrderRequest.Group = %q, want %q", g, tc.want)
			}
			if got.Complete != tc.complete {
				t.Errorf("SetOrderRequest.Complete = %v, want %v", got.Complete, tc.complete)
			}
		})
	}
}
