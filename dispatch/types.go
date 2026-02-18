package dispatch

const (
	OrderTypeRetrieve = "retrieve"
	OrderTypeMove     = "move"
	OrderTypeStore    = "store"

	StatusPending    = "pending"
	StatusSourcing   = "sourcing"
	StatusDispatched = "dispatched"
	StatusInTransit  = "in_transit"
	StatusDelivered  = "delivered"
	StatusConfirmed  = "confirmed"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
	StatusCancelled  = "cancelled"
)
