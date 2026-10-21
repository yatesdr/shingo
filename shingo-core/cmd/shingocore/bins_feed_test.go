package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"shingocore/engine"
)

// Every bus event the bins feed listens to reaches its hook with the bin, the
// nodes and the order it names, and nothing else does.
func TestWireBinsFeed_EventsReachTheHooks(t *testing.T) {
	t.Parallel()
	bus := engine.NewEventBus()
	var got []string
	record := func(args ...any) { got = append(got, strings.TrimSuffix(fmt.Sprintln(args...), "\n")) }
	wireBinsFeed(bus, binsFeedHooks{
		changed: func(binID int64, nodeIDs ...int64) { record("changed", binID, nodeIDs) },
		started: func(orderID int64, names ...string) { record("started", orderID, names) },
		ended:   func(orderID int64) { record("ended", orderID) },
	})
	for _, ev := range []engine.Event{
		{Type: engine.EventBinUpdated, Payload: engine.BinUpdatedEvent{BinID: 7, NodeID: 3, FromNodeID: 2, ToNodeID: 3}},
		{Type: engine.EventBinEnteredTransit, Payload: engine.BinEnteredTransitEvent{BinID: 9, OrderID: 40, FromNodeID: 5}},
		{Type: engine.EventOrderReceived, Payload: engine.OrderReceivedEvent{OrderID: 41, DeliveryNode: "KD-1"}},
		{Type: engine.EventOrderDispatched, Payload: engine.OrderDispatchedEvent{OrderID: 41, SourceNode: "SMN_A1", DestNode: "KD-1"}},
		{Type: engine.EventOrderCompleted, Payload: engine.OrderCompletedEvent{OrderID: 41}},
		{Type: engine.EventOrderFailed, Payload: engine.OrderFailedEvent{OrderID: 42}},
		{Type: engine.EventOrderCancelled, Payload: engine.OrderCancelledEvent{OrderID: 43}},
		{Type: engine.EventOrderSkipped, Payload: engine.OrderSkippedEvent{OrderID: 44}},
		{Type: engine.EventNodeUpdated, Payload: engine.NodeUpdatedEvent{NodeID: 3}},
	} {
		bus.Emit(ev)
	}
	want := []string{
		"changed 7 [3 2 3]",
		"changed 9 [5]",
		"started 41 [KD-1]",
		"started 41 [SMN_A1 KD-1]",
		"ended 41", "ended 42", "ended 43", "ended 44",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hooks called =\n %q\nwant\n %q", got, want)
	}
}
