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

// TestCreateOrder_ThreadsKeyRoute pins the keyRoute conduit on the unified create
// primitive: a KeyRoute on the request must land in rds.SetOrderRequest.KeyRoute
// (SEER's create-time robot-routing hint), and an unset KeyRoute must stay empty
// (json:"keyRoute,omitempty" omits it on the wire — no regression; empty keyRoute
// == SEER auto-picks, today's behavior). The populator (filling KeyRoute from each
// node's designated LMs) is a separate follow-on; this pins only the conduit.
func TestCreateOrder_ThreadsKeyRoute(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		route []string
		want  []string
	}{
		{"explicit key route threads to the wire", []string{"LM10", "LM11"}, []string{"LM10", "LM11"}},
		{"nil key route stays empty (SEER auto-picks)", nil, nil},
		{"empty key route stays empty", []string{}, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, captured := captureSetOrder(t)
			defer srv.Close()

			adapter := New(Config{BaseURL: srv.URL, Timeout: 5 * time.Second, DebugLog: func(string, ...any) {}})
			if _, err := adapter.CreateOrder(fleet.CreateOrderRequest{
				OrderID:    "sg-2-bbb",
				ExternalID: "uuid-2",
				Blocks:     []fleet.OrderBlock{{BlockID: "b1", Location: "LINE-01", BinTask: "JackLoad"}},
				KeyRoute:   tc.route,
				Complete:   true,
			}); err != nil {
				t.Fatalf("CreateOrder: %v", err)
			}
			got := captured()
			// Decode normalizes an absent keyRoute to a nil slice; compare len + elements.
			if len(got.KeyRoute) != len(tc.want) {
				t.Fatalf("SetOrderRequest.KeyRoute length = %d (%v), want %d (%v)", len(got.KeyRoute), got.KeyRoute, len(tc.want), tc.want)
			}
			for i, lm := range tc.want {
				if got.KeyRoute[i] != lm {
					t.Errorf("SetOrderRequest.KeyRoute[%d] = %q, want %q", i, got.KeyRoute[i], lm)
				}
			}
		})
	}
}

// TestCreateOrder_ThreadsKeyTask pins the keyTask conduit: a KeyTask on the
// request must land verbatim in rds.SetOrderRequest.KeyTask (the manual's literal
// "load"/"unload" robot-selection hint), and an empty KeyTask must stay empty
// (json:"keyTask,omitempty" omits it on the wire — no regression).
func TestCreateOrder_ThreadsKeyTask(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		task string
		want string
	}{
		{"load hint threads to the wire", "load", "load"},
		{"unload hint threads to the wire", "unload", "unload"},
		{"empty task stays empty (SEER auto-picks)", "", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, captured := captureSetOrder(t)
			defer srv.Close()

			adapter := New(Config{BaseURL: srv.URL, Timeout: 5 * time.Second, DebugLog: func(string, ...any) {}})
			if _, err := adapter.CreateOrder(fleet.CreateOrderRequest{
				OrderID:    "sg-3-ccc",
				ExternalID: "uuid-3",
				Blocks:     []fleet.OrderBlock{{BlockID: "b1", Location: "LINE-01", BinTask: "JackLoad"}},
				KeyTask:    tc.task,
				Complete:   true,
			}); err != nil {
				t.Fatalf("CreateOrder: %v", err)
			}
			got := captured()
			if got.KeyTask != tc.want {
				t.Errorf("SetOrderRequest.KeyTask = %q, want %q", got.KeyTask, tc.want)
			}
		})
	}
}
