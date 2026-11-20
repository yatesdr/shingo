package fleet

import "time"

// OrderTracker tracks active vendor orders and emits status change events.
type OrderTracker interface {
	Track(vendorOrderID string)
	Untrack(vendorOrderID string)
	ActiveCount() int
	Start()
	Stop()
}

// TrackerEmitter receives state transition events from a tracker.
//
// EmitBlockCompleted fires when an individual block within an order
// transitions to FINISHED while the parent order is still mid-flight.
// This is the per-pickup signal the bin-transit-state design needs.
// `binTask` carries the vendor's `binTask` field as reported (the key the
// block ran: JackLoad, JackUnload, Wait, a load sequence's names), for the
// block's leg row. The engine handler routes by its own record of what the
// block was sent to do, found by blockID, not by this name.
type TrackerEmitter interface {
	EmitOrderStatusChanged(orderID int64, vendorOrderID, oldStatus, newStatus, robotID, detail string, snapshot *OrderSnapshot)
	EmitBlockCompleted(orderID int64, vendorOrderID, blockID, location, binTask string, startTime, terminateTime int64)
	EmitGraceExpired(orderID int64, vendorOrderID string)
}

// UnreadableEmitter is the OPTIONAL half of TrackerEmitter: an order the
// fleet cannot report on — one order's reads failing for a while as others read
// fine (rds.UnreadableAfter) — and the read coming back. Optional so emitters
// that do not care need not grow it.
type UnreadableEmitter interface {
	EmitOrderUnreadable(orderID int64, vendorOrderID string, since time.Time, reason string)
	EmitOrderReadable(orderID int64, vendorOrderID string)
}

// RobotChangeEmitter is the OPTIONAL half of TrackerEmitter for a robot the
// fleet moved an order to without changing the order's state (vendorState is
// that unchanged state). It is not a status change and must not be read as
// one: see rds.Poller's reportRobotSwap. Optional, like UnreadableEmitter.
type RobotChangeEmitter interface {
	EmitOrderRobotChanged(orderID int64, vendorOrderID, vendorState, fromRobotID, robotID string)
}

// OrderIDResolver maps vendor order IDs back to ShinGo order IDs.
type OrderIDResolver interface {
	ResolveVendorOrderID(vendorOrderID string) (int64, error)
}

// TrackingBackend is a Backend that also provides order tracking.
type TrackingBackend interface {
	Backend
	InitTracker(emitter TrackerEmitter, resolver OrderIDResolver)
	Tracker() OrderTracker
}
