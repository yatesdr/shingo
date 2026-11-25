package engine

import (
	"shingo/protocol/eventbus"
)

// bins_feed.go — the bus events that can change what an Edge's watched nodes
// hold, pointed at the bins feed's push (messaging/bins_feed.go). No new event
// type: a bin event names the bin and the nodes it touched; a lift names the
// bin and the node it left; an order is seen
// when it is received and dispatched (its delivery, then its source and
// destination) and when it ends, however it ends.
//
// main subscribes these on messaging's behalf, after Start and before the web
// layer's (cmd/shingocore/main.go), so they come after every subscription of
// the engine's own and before the screens' (TestSubscriberCensus). They are a
// method on the engine so that a harness can make the same eight.
//
// The subscribers do map work and arm a timer; the bus is synchronous and they
// run inside the emitter.

// BinsFeedHooks are the three entry points of the bins feed's push.
type BinsFeedHooks struct {
	Changed func(binID int64, nodeIDs ...int64)
	Started func(orderID int64, nodeNames ...string)
	Ended   func(orderID int64)
}

// SubscribeBinsFeed subscribes h to the eight bus events. Call it once.
func (e *Engine) SubscribeBinsFeed(h BinsFeedHooks) {
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, BinUpdatedEvent]) {
		ev := evt.Payload
		h.Changed(ev.BinID, ev.NodeID, ev.FromNodeID, ev.ToNodeID)
	}, EventBinUpdated)
	// The lift: the carrier left its node, and its generation ended with it
	// (LiftToTransitTx). The node it left is the one that changed.
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, BinEnteredTransitEvent]) {
		h.Changed(evt.Payload.BinID, evt.Payload.FromNodeID)
	}, EventBinEnteredTransit)
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderReceivedEvent]) {
		h.Started(evt.Payload.OrderID, evt.Payload.DeliveryNode)
	}, EventOrderReceived)
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderDispatchedEvent]) {
		h.Started(evt.Payload.OrderID, evt.Payload.SourceNode, evt.Payload.DestNode)
	}, EventOrderDispatched)
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderCompletedEvent]) {
		h.Ended(evt.Payload.OrderID)
	}, EventOrderCompleted)
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderFailedEvent]) {
		h.Ended(evt.Payload.OrderID)
	}, EventOrderFailed)
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderCancelledEvent]) {
		h.Ended(evt.Payload.OrderID)
	}, EventOrderCancelled)
	eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderSkippedEvent]) {
		h.Ended(evt.Payload.OrderID)
	}, EventOrderSkipped)
}
