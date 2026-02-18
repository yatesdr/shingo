package dispatch

// Emitter is the interface adapters must satisfy to bridge dispatch events to the engine.
type Emitter interface {
	EmitOrderReceived(orderID int64, edgeUUID, clientID, orderType, payloadTypeCode, deliveryNode string)
	EmitOrderDispatched(orderID int64, rdsOrderID, sourceNode, destNode string)
	EmitOrderFailed(orderID int64, edgeUUID, clientID, errorCode, detail string)
	EmitOrderCancelled(orderID int64, edgeUUID, clientID, reason string)
	EmitOrderCompleted(orderID int64, edgeUUID, clientID string)
}
