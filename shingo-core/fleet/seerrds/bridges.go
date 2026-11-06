package seerrds

import (
	"time"

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

func (b *emitterBridge) EmitGraceExpired(orderID int64, rdsOrderID string) {
	b.emitter.EmitGraceExpired(orderID, rdsOrderID)
}

func (b *emitterBridge) EmitBlockCompleted(orderID int64, rdsOrderID, blockID, location, binTask string, startTime, terminateTime int64) {
	b.emitter.EmitBlockCompleted(orderID, rdsOrderID, blockID, location, binTask, startTime, terminateTime)
}

// EmitOrderUnreadable / EmitOrderReadable forward rds.UnreadableEmitter to the
// fleet layer's optional fleet.UnreadableEmitter, when the emitter has it.
func (b *emitterBridge) EmitOrderUnreadable(orderID int64, rdsOrderID string, since time.Time, reason string) {
	if ue, ok := b.emitter.(fleet.UnreadableEmitter); ok {
		ue.EmitOrderUnreadable(orderID, rdsOrderID, since, reason)
	}
}

func (b *emitterBridge) EmitOrderReadable(orderID int64, rdsOrderID string) {
	if ue, ok := b.emitter.(fleet.UnreadableEmitter); ok {
		ue.EmitOrderReadable(orderID, rdsOrderID)
	}
}

var _ rds.UnreadableEmitter = (*emitterBridge)(nil)

// resolverBridge adapts fleet.OrderIDResolver to rds.OrderIDResolver.
type resolverBridge struct {
	resolver fleet.OrderIDResolver
}

func (b *resolverBridge) ResolveRDSOrderID(rdsOrderID string) (int64, error) {
	return b.resolver.ResolveVendorOrderID(rdsOrderID)
}
