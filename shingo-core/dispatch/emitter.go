package dispatch

// Emitter is the interface adapters must satisfy to bridge dispatch events to the engine.
type Emitter interface {
	EmitOrderReceived(orderID int64, edgeUUID, stationID, orderType, styleCode, deliveryNode string)
	EmitOrderDispatched(orderID int64, vendorOrderID, sourceNode, destNode string)
	EmitOrderFailed(orderID int64, edgeUUID, stationID, errorCode, detail string)
	EmitOrderCancelled(orderID int64, edgeUUID, stationID, reason string)
	EmitOrderCompleted(orderID int64, edgeUUID, stationID string)
}
