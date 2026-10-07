package www

import (
	"testing"

	"shingocore/config"
	"shingocore/engine"
	"shingocore/service"
)

// TestPinSSE_RobotFeedBuilds counts the robot feed's order-line builds per
// fleet tick, by who is connected (LC8).
//
// Before LC8 the build ran whenever ClientCount() > 0, so a tab filtered to
// other topics paid for a payload it was never sent: 0 / 1 / 1 / 1. After, it
// runs only when a client would receive robot-update: 0 / 0 / 1 / 1.
func TestPinSSE_RobotFeedBuilds(t *testing.T) {
	builds := 0
	orig := buildRobotOrderLines
	buildRobotOrderLines = func(*service.OrderService, *config.Config) map[string]RobotOrderLine {
		builds++
		return map[string]RobotOrderLine{}
	}
	defer func() { buildRobotOrderLines = orig }()

	eng := &engine.Engine{Events: engine.NewEventBus()}
	hub := NewEventHub()
	hub.Start()
	defer hub.Stop()
	hub.SetupEngineListeners(eng)

	tick := func() int {
		builds = 0
		// Emit is synchronous: the listener has run when it returns.
		eng.Events.Emit(engine.Event{Type: engine.EventRobotsUpdated, Payload: engine.RobotsUpdatedEvent{}})
		return builds
	}

	cases := []struct {
		name  string
		add   func() *sseClient
		after int
	}{
		{"no client", nil, 0},
		{"one client not subscribed", func() *sseClient { return hub.AddClientFiltered([]string{"system-status"}) }, 0}, // before: 1
		{"one client subscribed", func() *sseClient { return hub.AddClientFiltered([]string{"robot-update"}) }, 1},
		{"one unfiltered client", hub.AddClient, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.add != nil {
				c := tc.add()
				defer hub.RemoveClient(c)
			}
			if got := tick(); got != tc.after {
				t.Errorf("builds per tick = %d, want %d", got, tc.after)
			}
		})
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
