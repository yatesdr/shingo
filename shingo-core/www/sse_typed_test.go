package www

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/engine"
	"shingocore/fleet"
)

// TestSetupEngineListeners_TypedBroadcast guards the SubscribeTyped migration in
// sse.go. Each payload-carrying listener now binds its event type to a concrete
// payload type P via eventbus.SubscribeTyped. A wrong event-type↔payload-type
// pairing still COMPILES (both are valid types) but is skipped at runtime with a
// logged "payload mismatch" — so the broadcast silently never fires. Emitting
// each real payload and requiring its SSE frame is the regression guard against
// exactly that mis-binding.
//
// The hub is wired to a bare engine carrying only an event bus: SetupEngineListeners
// just registers subscribers, and the events asserted here never reach the DB /
// robot-cache branches (status is non-"in_transit", RobotID is empty), so no
// engine.Start() or DB is needed.
func TestSetupEngineListeners_TypedBroadcast(t *testing.T) {
	eng := &engine.Engine{Events: engine.NewEventBus()}

	hub := NewEventHub()
	hub.Start()
	defer hub.Stop()
	hub.SetupEngineListeners(eng)

	ch := hub.AddClient()
	defer hub.RemoveClient(ch)

	cases := []struct {
		name      string
		typ       engine.EventType
		payload   any
		wantEvent string
		wantSub   string // substring required in frame Data ("" = match event name only)
	}{
		{"received", engine.EventOrderReceived, engine.OrderReceivedEvent{OrderID: 1}, "order-update", `"type":"received"`},
		{"dispatched", engine.EventOrderDispatched, engine.OrderDispatchedEvent{OrderID: 2, VendorOrderID: "V2"}, "order-update", `"type":"dispatched"`},
		{"status_changed", engine.EventOrderStatusChanged, engine.OrderStatusChangedEvent{OrderID: 3, NewStatus: "queued"}, "order-update", `"type":"status_changed"`},
		{"mission_event", engine.EventOrderStatusChanged, engine.OrderStatusChangedEvent{OrderID: 3, NewStatus: "queued"}, "mission-event", ""},
		{"completed", engine.EventOrderCompleted, engine.OrderCompletedEvent{OrderID: 4}, "order-update", `"type":"completed"`},
		{"failed", engine.EventOrderFailed, engine.OrderFailedEvent{OrderID: 5, Detail: "x"}, "order-update", `"type":"failed"`},
		{"cancelled", engine.EventOrderCancelled, engine.OrderCancelledEvent{OrderID: 6, Reason: "x"}, "order-update", `"type":"cancelled"`},
		{"skipped", engine.EventOrderSkipped, engine.OrderSkippedEvent{OrderID: 7, Detail: "x"}, "order-update", `"type":"skipped"`},
		{"bin", engine.EventBinUpdated, engine.BinUpdatedEvent{NodeID: 9, Action: engine.BinActionMoved, BinID: 1}, "bin-update", ""},
		{"lift", engine.EventBinEnteredTransit, engine.BinEnteredTransitEvent{BinID: 2, FromNodeID: 9}, "bin-update", `"action":"lifted"`},
		{"bin_row", engine.EventBinRowChanged, engine.BinRowChangedEvent{BinID: 3}, "bin-row", `"bin_id":3`},

		{"node", engine.EventNodeUpdated, engine.NodeUpdatedEvent{NodeID: 11, Action: "updated"}, "node-update", ""},
		{"cms", engine.EventCMSTransaction, engine.CMSTransactionEvent{}, "cms-transaction", ""},
		{"robots", engine.EventRobotsUpdated, engine.RobotsUpdatedEvent{}, "robot-update", ""},
		{"cell", engine.EventCellTick, engine.CellTickEvent{Station: "S1", ProcessID: 1}, "cell-heartbeat", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drainFrames(ch)
			eng.Events.Emit(engine.Event{Type: tc.typ, Payload: tc.payload})
			if !waitForFrame(ch, tc.wantEvent, tc.wantSub, 2*time.Second) {
				t.Fatalf("emitting event type %v produced no %q frame (data sub %q) — "+
					"SubscribeTyped most likely skipped it on a payload-type mismatch",
					tc.typ, tc.wantEvent, tc.wantSub)
			}
		})
	}
}

// drainFrames clears any frames left from a prior case so the next assertion
// only sees frames from its own emit. Both queues are drained: a subscriber
// receives coalescing topics on one and everything else on the other.
func drainFrames(c *sseClient) {
	for {
		select {
		case <-c.durable:
		case <-c.wake:
			c.takeCoalesced()
		default:
			return
		}
	}
}

// waitForFrame reads frames until one matches event (and, if sub != "", whose
// Data contains sub), or the timeout elapses. Non-matching frames — e.g. the
// mission-event that also fires on a status change — are skipped.
func waitForFrame(c *sseClient, event, sub string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	match := func(evt SSEEvent) bool {
		return evt.Event == event && (sub == "" || strings.Contains(evt.Data, sub))
	}
	for {
		select {
		case evt := <-c.durable:
			if match(evt) {
				return true
			}
		case <-c.wake:
			if slices.ContainsFunc(c.takeCoalesced(), match) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// frameData reads frames until one of event arrives and returns its Data, or
// false when the timeout elapses first.
func frameData(c *sseClient, event string, timeout time.Duration) (string, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case evt := <-c.durable:
			if evt.Event == event {
				return evt.Data, true
			}
		case <-c.wake:
			for _, evt := range c.takeCoalesced() {
				if evt.Event == event {
					return evt.Data, true
				}
			}
		case <-deadline:
			return "", false
		}
	}
}

// TestRobotUpdateFrame_CarriesThePlannedRoute pins the route on the fleet
// frame the Core map reads: a driving robot's "path" is its Path, scene names
// in order, and a robot with no route has no "path" key at all, so "no route"
// costs nothing on the wire and cannot be mistaken for an empty one.
//
// It also logs what the route costs per robot per tick, on the frame and on
// the /api/robots entry (which is fleet.RobotStatus as encoded): with and
// without a 22-point route, the length of the shared-map study's first drive.
// Run with -v to read them; nothing asserts the count.
func TestRobotUpdateFrame_CarriesThePlannedRoute(t *testing.T) {
	eng := &engine.Engine{Events: engine.NewEventBus()}
	hub := NewEventHub()
	hub.Start()
	defer hub.Stop()
	hub.SetupEngineListeners(eng)
	ch := hub.AddClientFiltered([]string{"robot-update"})
	defer hub.RemoveClient(ch)

	route := make([]string, 22) // invented names, five characters each
	for i := range route {
		route[i] = fmt.Sprintf("LM%d", 101+i)
	}
	robot := func(id string, path []string) fleet.RobotStatus {
		return fleet.RobotStatus{VehicleID: id, Connected: true, Busy: true, X: 12.345, Y: -6.789, Angle: 1.571,
			BatteryLevel: 87, CurrentMap: "map-1", Model: "model-1", IP: "192.0.2.1",
			PathIDs: []string{"101", "102"}, Path: path}
	}
	frame := func(robots ...fleet.RobotStatus) string {
		t.Helper()
		drainFrames(ch)
		eng.Events.Emit(engine.Event{Type: engine.EventRobotsUpdated, Payload: engine.RobotsUpdatedEvent{Robots: robots}})
		data, ok := frameData(ch, "robot-update", 2*time.Second)
		if !ok {
			t.Fatal("no robot-update frame")
		}
		return data
	}

	var got []map[string]json.RawMessage
	testutil.MustNoErr(t, json.Unmarshal([]byte(frame(robot("AMR-01", route[:3]), robot("AMR-02", nil))), &got), "decode frame")
	if len(got) != 2 {
		t.Fatalf("frame has %d robots, want 2", len(got))
	}
	if p := string(got[0]["path"]); p != `["LM101","LM102","LM103"]` {
		t.Errorf("AMR-01 path = %s, want [\"LM101\",\"LM102\",\"LM103\"] (in order)", p)
	}
	if _, has := got[1]["path"]; has {
		t.Errorf("AMR-02 has no route and still carries \"path\": %s", got[1]["path"])
	}
	for _, r := range got {
		for k := range r {
			if strings.Contains(strings.ToLower(k), "pathid") {
				t.Errorf("the frame carries the fleet's raw route ids as %q", k)
			}
		}
	}

	with, without := len(frame(robot("AMR-01", route))), len(frame(robot("AMR-01", nil)))
	apiWith, err := json.Marshal(robot("AMR-01", route))
	testutil.MustNoErr(t, err, "marshal")
	apiWithout, err := json.Marshal(robot("AMR-01", nil))
	testutil.MustNoErr(t, err, "marshal")
	t.Logf("robot-update, one robot: %d bytes without a route, %d with 22 points: +%d (%.1f per point)",
		without, with, with-without, float64(with-without)/22)
	t.Logf("/api/robots entry: %d bytes without a route, %d with 22 points: +%d (%.1f per point)",
		len(apiWithout), len(apiWith), len(apiWith)-len(apiWithout), float64(len(apiWith)-len(apiWithout))/22)
}
