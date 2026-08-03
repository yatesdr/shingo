package dispatch

import (
	"shingo/protocol"
	"shingocore/store/orders"
)

// order_projection.go — turning a Core order row into the shape the Edge stores.
//
// One conversion, used by both delivery paths: the push Core sends when it
// authors an order, and the reconcile reply that carries the same rows back for
// anything the Edge is missing. Two conversions would be two answers to "what is
// this order", and the reconcile exists precisely to correct disagreements.

// ProjectionFor renders an order as the row the Edge should hold.
//
// process_node_id is absent by design, not by omission: it is an Edge-local
// foreign key that Core has never had. The Edge resolves it from DeliveryNode
// and tolerates not finding one.
func ProjectionFor(o *orders.Order) protocol.OrderProjection {
	if o == nil {
		return protocol.OrderProjection{}
	}
	return protocol.OrderProjection{
		OrderUUID:    o.EdgeUUID,
		OrderType:    o.OrderType,
		Status:       string(o.Status),
		StationID:    o.StationID,
		Quantity:     int64(o.Quantity),
		SourceNode:   o.SourceNode,
		DeliveryNode: o.DeliveryNode,
		PayloadCode:  o.PayloadCode,
		PayloadDesc:  o.PayloadDesc,
		// The Edge keeps this as its own column rather than deriving it from the
		// type. Sent explicitly for the same reason: the two have drifted before.
		RetrieveEmpty: o.OrderType == OrderTypeRetrieveEmpty,
		OriginID:      o.OriginID,
		OriginClass:   o.OriginClass,
		QueueReason:   o.QueueReason,
		QueueCode:     o.QueueCode,
	}
}
