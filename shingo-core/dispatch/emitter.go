package dispatch

import "shingo/protocol"

// Emitter is the interface adapters must satisfy to bridge dispatch events to the engine.
type Emitter interface {
	EmitOrderReceived(orderID int64, edgeUUID, stationID string, orderType protocol.OrderType, payloadCode, deliveryNode string)
	EmitOrderDispatched(orderID int64, vendorOrderID, sourceNode, destNode string)
	EmitOrderFailed(orderID int64, edgeUUID, stationID, errorCode, detail string)
	EmitOrderSkipped(orderID int64, edgeUUID, stationID, errorCode, detail string)
	EmitOrderCancelled(orderID int64, edgeUUID, stationID, reason, previousStatus string)
	EmitOrderCompleted(orderID int64, edgeUUID, stationID string)
	EmitOrderQueued(orderID int64, edgeUUID, stationID, payloadCode string)
	EmitOrderFaulted(orderID int64, edgeUUID, stationID, reason string)
	EmitOrderFaultedRecovered(orderID int64, edgeUUID, stationID, robotID string)

	// ProjectOrder pushes a whole order row down to the station that owns it, so
	// an order Core authored appears on that Edge's board. Unlike the Emit
	// methods above, which publish to Core's in-process event bus, this one goes
	// on the wire — hence the different verb. It is best-effort by design: the
	// outbox drops a message permanently once it is past its retries, and the
	// order status reconcile is what repairs a projection that never lands.
	ProjectOrder(stationID string, projection protocol.OrderProjection)
}
