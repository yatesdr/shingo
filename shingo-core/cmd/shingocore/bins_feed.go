package main

import (
	"shingo/protocol/eventbus"
	"shingocore/engine"
)

// bins_feed.go — the bus events that can change what an Edge's watched nodes
// hold, pointed at the bins feed's push (messaging/bins_feed.go). No new event
// type: a bin event names the bin and the nodes it touched; a lift names the
// bin and the node it left; an order is seen
// when it is received and dispatched (its delivery, then its source and
// destination) and when it ends, however it ends.
//
// The subscribers do map work and arm a timer; the bus is synchronous and they
// run inside the emitter.

// binsFeedHooks are the three entry points of the bins feed's push.
type binsFeedHooks struct {
	changed func(binID int64, nodeIDs ...int64)
	started func(orderID int64, nodeNames ...string)
	ended   func(orderID int64)
}

func wireBinsFeed(bus *engine.EventBus, h binsFeedHooks) {
	eventbus.SubscribeTyped(bus, func(evt eventbus.TypedEvent[engine.EventType, engine.BinUpdatedEvent]) {
		ev := evt.Payload
		h.changed(ev.BinID, ev.NodeID, ev.FromNodeID, ev.ToNodeID)
	}, engine.EventBinUpdated)
	// The lift: the carrier left its node, and its generation ended with it
	// (LiftToTransitTx). The node it left is the one that changed.
	eventbus.SubscribeTyped(bus, func(evt eventbus.TypedEvent[engine.EventType, engine.BinEnteredTransitEvent]) {
		h.changed(evt.Payload.BinID, evt.Payload.FromNodeID)
	}, engine.EventBinEnteredTransit)
	eventbus.SubscribeTyped(bus, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderReceivedEvent]) {
		h.started(evt.Payload.OrderID, evt.Payload.DeliveryNode)
	}, engine.EventOrderReceived)
	eventbus.SubscribeTyped(bus, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderDispatchedEvent]) {
		h.started(evt.Payload.OrderID, evt.Payload.SourceNode, evt.Payload.DestNode)
	}, engine.EventOrderDispatched)
	eventbus.SubscribeTyped(bus, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderCompletedEvent]) {
		h.ended(evt.Payload.OrderID)
	}, engine.EventOrderCompleted)
	eventbus.SubscribeTyped(bus, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderFailedEvent]) {
		h.ended(evt.Payload.OrderID)
	}, engine.EventOrderFailed)
	eventbus.SubscribeTyped(bus, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderCancelledEvent]) {
		h.ended(evt.Payload.OrderID)
	}, engine.EventOrderCancelled)
	eventbus.SubscribeTyped(bus, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderSkippedEvent]) {
		h.ended(evt.Payload.OrderID)
	}, engine.EventOrderSkipped)
}
