package fleet

// OrderTracker tracks active vendor orders and emits status change events.
type OrderTracker interface {
	Track(vendorOrderID string)
	Untrack(vendorOrderID string)
	ActiveCount() int
	Start()
	Stop()
}

// TrackerEmitter receives state transition events from a tracker.
type TrackerEmitter interface {
	EmitOrderStatusChanged(orderID int64, vendorOrderID, oldStatus, newStatus, robotID, detail string, snapshot *OrderSnapshot)
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
