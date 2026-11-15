package www

import (
	"strings"
	"testing"
	"time"

	"shingocore/config"
	"shingocore/engine"
	"shingocore/fleet"
	"shingocore/service"
)

// TestPinSSE_RobotFeedBuilds counts, per fleet tick and by who is connected,
// what the robot feed builds: its order lines, and the frame itself (a
// robotJSON per robot, marshalled and handed to the hub).
//
// Before LC8 the order-line build ran whenever ClientCount() > 0, so a tab
// filtered to other topics paid for a payload it was never sent: 0 / 1 / 1 / 1.
// After, it runs only when a client would receive robot-update: 0 / 0 / 1 / 1.
//
// LC8 guarded only the order lines. The frame was still built, marshalled and
// handed to the hub on every tick with nobody to send it to: 1 / 1 / 1 / 1.
// Since 2026-11-14 the whole build sits behind the same question: 0 / 0 / 1 / 1.
//
// A frame is counted where it lands, on the hub's broadcast queue; the hub is
// not started, so nothing drains it. In the listener the robotJSON loop, the
// marshal (sseJSON, evaluated as Broadcast's argument) and the hand-off are one
// straight line with no return between them, so a frame on the queue is one
// built and marshalled, and none on the queue is none built.
func TestPinSSE_RobotFeedBuilds(t *testing.T) {
	builds := 0
	orig := buildRobotOrderLines
	buildRobotOrderLines = func(*service.OrderService, *config.Config) map[string]RobotOrderLine {
		builds++
		return map[string]RobotOrderLine{}
	}
	defer func() { buildRobotOrderLines = orig }()

	eng := &engine.Engine{Events: engine.NewEventBus()}
	hub := NewEventHub() // not started: the frames stay on hub.broadcast, where they are counted
	hub.SetupEngineListeners(eng)
	fleetTick := engine.RobotsUpdatedEvent{Robots: []fleet.RobotStatus{
		{VehicleID: "AMR-01", Connected: true}, {VehicleID: "AMR-02", Connected: true},
	}}

	tick := func() (int, []SSEEvent) {
		builds = 0
		// Emit is synchronous: the listener has run when it returns.
		eng.Events.Emit(engine.Event{Type: engine.EventRobotsUpdated, Payload: fleetTick})
		return builds, drainBroadcast(hub)
	}

	cases := []struct {
		name   string
		add    func() *sseClient
		lines  int
		frames int
	}{
		{"no client", nil, 0, 0}, // frames before 2026-11-14: 1
		{"one client not subscribed", func() *sseClient { return hub.AddClientFiltered([]string{"system-status"}) }, 0, 0}, // lines before LC8: 1; frames before 2026-11-14: 1
		{"one client subscribed", func() *sseClient { return hub.AddClientFiltered([]string{"robot-update"}) }, 1, 1},
		{"one unfiltered client", hub.AddClient, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.add != nil {
				c := tc.add()
				defer hub.RemoveClient(c)
			}
			lines, frames := tick()
			if lines != tc.lines {
				t.Errorf("order-line builds per tick = %d, want %d", lines, tc.lines)
			}
			if len(frames) != tc.frames {
				t.Fatalf("frames built and marshalled per tick = %d, want %d", len(frames), tc.frames)
			}
			for _, f := range frames {
				if f.Event != "robot-update" || !strings.Contains(f.Data, `"vehicle_id":"AMR-01"`) ||
					!strings.Contains(f.Data, `"vehicle_id":"AMR-02"`) {
					t.Errorf("frame %s %s, want robot-update carrying the whole fleet", f.Event, f.Data)
				}
			}
		})
	}
}

// TestPinSSE_RobotFeedSeesOnsetsWithNoReceiver: the alarm onsets are observed
// on every tick, receivers or not, so a tab opened after a fault is still told
// which alarm began with it. The no-receiver return sits just below the
// observe; this fails if it ever moves above it.
func TestPinSSE_RobotFeedSeesOnsetsWithNoReceiver(t *testing.T) {
	since := time.Now().UTC()
	orig := buildRobotOrderLines
	buildRobotOrderLines = func(*service.OrderService, *config.Config) map[string]RobotOrderLine {
		return map[string]RobotOrderLine{"AMR-01": {OrderID: 9, OrderStatus: "faulted", FaultSince: &since}}
	}
	defer func() { buildRobotOrderLines = orig }()

	eng := &engine.Engine{Events: engine.NewEventBus()}
	hub := NewEventHub() // not started, as above
	hub.SetupEngineListeners(eng)
	tick := func(alarms ...fleet.RobotAlarm) []SSEEvent {
		r := fleet.RobotStatus{VehicleID: "AMR-01", Connected: true, Alarms: alarms}
		eng.Events.Emit(engine.Event{Type: engine.EventRobotsUpdated,
			Payload: engine.RobotsUpdatedEvent{Robots: []fleet.RobotStatus{r}}})
		return drainBroadcast(hub)
	}
	blocked := fleet.RobotAlarm{Code: 52200, Severity: "error", Desc: "robot is blocked"}

	// Nobody watching: the robot is first seen clean, then the alarm begins.
	if f := tick(); len(f) != 0 {
		t.Fatalf("no receiver: %d frames, want 0", len(f))
	}
	if f := tick(blocked); len(f) != 0 {
		t.Fatalf("no receiver: %d frames, want 0", len(f))
	}
	// A tab opens after the fault.
	c := hub.AddClient()
	defer hub.RemoveClient(c)
	f := tick(blocked)
	if len(f) != 1 || !strings.Contains(f[0].Data, `"fault_alarm":{"code":52200`) {
		t.Fatalf("first frame after the tab opened = %v, want AMR-01's fault alarm 52200, "+
			"whose onset was seen while nobody was watching", f)
	}
}

// drainBroadcast takes every frame waiting on an unstarted hub's queue.
func drainBroadcast(h *EventHub) []SSEEvent {
	var out []SSEEvent
	for {
		select {
		case e := <-h.broadcast:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestHasSubscriber covers the hub question LC8 asks before building.
func TestHasSubscriber(t *testing.T) {
	hub := NewEventHub()
	if hub.HasSubscriber("robot-update") {
		t.Fatal("no clients: want false")
	}
	other := hub.AddClientFiltered([]string{"order-update", "system-status"})
	if hub.HasSubscriber("robot-update") {
		t.Error("only a client filtered to other topics: want false")
	}
	if !hub.HasSubscriber("order-update") {
		t.Error("a client filtered to order-update: want true for order-update")
	}
	all := hub.AddClient()
	if !hub.HasSubscriber("robot-update") {
		t.Error("an unfiltered client: want true")
	}
	hub.RemoveClient(all)
	hub.RemoveClient(other)
	if hub.HasSubscriber("order-update") {
		t.Error("all clients removed: want false")
	}
}
