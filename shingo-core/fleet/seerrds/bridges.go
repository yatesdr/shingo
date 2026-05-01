package seerrds

import (
	"shingocore/fleet"
	"shingocore/rds"
)

// emitterBridge adapts fleet.TrackerEmitter to rds.PollerEmitter.
type emitterBridge struct {
	emitter fleet.TrackerEmitter
}

func (b *emitterBridge) EmitOrderStatusChanged(orderID int64, rdsOrderID, oldStatus, newStatus, robotID, detail string, orderDetail *rds.OrderDetail) {
	var snapshot *fleet.OrderSnapshot
	if orderDetail != nil {
		snapshot = mapOrderSnapshot(orderDetail)
	}
	b.emitter.EmitOrderStatusChanged(orderID, rdsOrderID, oldStatus, newStatus, robotID, detail, snapshot)
}

func (b *emitterBridge) EmitBlockCompleted(orderID int64, rdsOrderID, blockID, location, binTask string) {
	b.emitter.EmitBlockCompleted(orderID, rdsOrderID, blockID, location, binTask)
}

// resolverBridge adapts fleet.OrderIDResolver to rds.OrderIDResolver.
type resolverBridge struct {
	resolver fleet.OrderIDResolver
}

func (b *resolverBridge) ResolveRDSOrderID(rdsOrderID string) (int64, error) {
	return b.resolver.ResolveVendorOrderID(rdsOrderID)
}
