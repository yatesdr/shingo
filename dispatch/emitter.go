package dispatch

// Emitter is the interface adapters must satisfy to bridge dispatch events to the engine.
type Emitter interface {
	EmitOrderReceived(orderID int64, wardropUUID, clientID, orderType, materialCode, deliveryNode string)
	EmitOrderDispatched(orderID int64, rdsOrderID, sourceNode, destNode string)
	EmitOrderFailed(orderID int64, wardropUUID, clientID, errorCode, detail string)
	EmitOrderCancelled(orderID int64, wardropUUID, clientID, reason string)
	EmitOrderCompleted(orderID int64, wardropUUID, clientID string)
}
