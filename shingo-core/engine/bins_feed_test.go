package engine

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Every bus event the bins feed listens to reaches its hook with the bin, the
// nodes and the order it names, and nothing else does.
func TestSubscribeBinsFeed_EventsReachTheHooks(t *testing.T) {
	t.Parallel()
	e := &Engine{Events: NewEventBus()}
	var got []string
	record := func(args ...any) { got = append(got, strings.TrimSuffix(fmt.Sprintln(args...), "\n")) }
	e.SubscribeBinsFeed(BinsFeedHooks{
		Changed: func(binID int64, nodeIDs ...int64) { record("changed", binID, nodeIDs) },
		Started: func(orderID int64, names ...string) { record("started", orderID, names) },
		Ended:   func(orderID int64) { record("ended", orderID) },
	})
	for _, ev := range []Event{
		{Type: EventBinUpdated, Payload: BinUpdatedEvent{BinID: 7, NodeID: 3, FromNodeID: 2, ToNodeID: 3}},
		{Type: EventBinEnteredTransit, Payload: BinEnteredTransitEvent{BinID: 9, OrderID: 40, FromNodeID: 5}},
		{Type: EventOrderReceived, Payload: OrderReceivedEvent{OrderID: 41, DeliveryNode: "KD-1"}},
		{Type: EventOrderDispatched, Payload: OrderDispatchedEvent{OrderID: 41, SourceNode: "SMN_A1", DestNode: "KD-1"}},
		{Type: EventOrderCompleted, Payload: OrderCompletedEvent{OrderID: 41}},
		{Type: EventOrderFailed, Payload: OrderFailedEvent{OrderID: 42}},
		{Type: EventOrderCancelled, Payload: OrderCancelledEvent{OrderID: 43}},
		{Type: EventOrderSkipped, Payload: OrderSkippedEvent{OrderID: 44}},
		{Type: EventNodeUpdated, Payload: NodeUpdatedEvent{NodeID: 3}},
	} {
		e.Events.Emit(ev)
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
