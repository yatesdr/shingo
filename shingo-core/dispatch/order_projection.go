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
//
// THE TWO forbidigo EXEMPTIONS BELOW ARE PROJECTION, NOT ROUTING. The guard on
// order.OrderType exists because dispatch and fulfillment must not BRANCH on it
// — they route on plan provenance, SourceIntent, or resolvedStep data. This
// function decides nothing. It copies the order onto the wire so the Edge's
// board can render a row, and the Edge has always held both fields as its own
// columns. Reading the value to send it is the one use the rule is not about.
func ProjectionFor(o *orders.Order) protocol.OrderProjection {
	if o == nil {
		return protocol.OrderProjection{}
	}
	return protocol.OrderProjection{
		OrderUUID:    o.EdgeUUID,
		OrderType:    o.OrderType, //nolint:forbidigo // projected onto the wire, not branched on — see the doc comment
		Status:       string(o.Status),
		StationID:    o.StationID,
		Quantity:     int64(o.Quantity),
		SourceNode:   o.SourceNode,
		DeliveryNode: o.DeliveryNode,
		PayloadCode:  o.PayloadCode,
		PayloadDesc:  o.PayloadDesc,
		// The Edge keeps this as its own column rather than deriving it from the
		// type. Sent explicitly for the same reason: the two have drifted before.
		RetrieveEmpty: o.OrderType == OrderTypeRetrieveEmpty, //nolint:forbidigo // derives a wire field, not a route
		OriginID:      o.OriginID,
		OriginClass:   o.OriginClass,
		QueueReason:   o.QueueReason,
		QueueCode:     o.QueueCode,
	}
}
