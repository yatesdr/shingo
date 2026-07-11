package fleet

import "time"

// Backend is the vendor-neutral interface for fleet management systems.
// Implementations wrap vendor-specific APIs (Seer RDS, MiR, Locus, etc.).
type Backend interface {
	// CreateOrder creates a block-based order at the fleet backend. It is the
	// single create primitive for BOTH lifecycles: a no-wait order (all simple
	// traffic, and future single-shot complex) passes Complete=true so the fleet
	// completes the order once its blocks finish; a staged/waiting order passes
	// Complete=false and later appends further blocks (and flips completion) via
	// ReleaseOrder. The Complete field IS the lane-coordination (Part B) on-ramp:
	// a lane-dwell order creates Complete=false with a Wait block, then releases.
	// The two former primitives (CreateTransportOrder / CreateStagedOrder) are
	// re-expressed in terms of this one (see the adapter); they are gone from the
	// interface so a third create path can never drift.
	CreateOrder(req CreateOrderRequest) (TransportOrderResult, error)

	// CancelOrder cancels a previously dispatched order.
	CancelOrder(vendorOrderID string) error

	// SetOrderPriority changes the priority of an active order.
	SetOrderPriority(vendorOrderID string, priority int) error

	// Ping checks connectivity to the fleet backend.
	Ping() error

	// Name returns a human-readable name for this backend (e.g. "SEER RDS").
	Name() string

	// MapState translates a vendor-specific state string to a dispatch status.
	MapState(vendorState string) string

	// IsTerminalState returns true if the vendor state represents a terminal state.
	IsTerminalState(vendorState string) bool

	// ReleaseOrder appends blocks to a staged order. When complete is true the
	// order is marked finished (no more blocks will follow). When complete is
	// false the order stays staged so the robot can dwell at the next wait point.
	ReleaseOrder(vendorOrderID string, blocks []OrderBlock, complete bool) error

	// Reconfigure applies configuration changes at runtime.
	Reconfigure(cfg ReconfigureParams)
}

// ReconfigureParams holds typed configuration for fleet runtime reconfiguration.
type ReconfigureParams struct {
	BaseURL string
	Timeout time.Duration
}

// CreateOrderRequest contains the vendor-neutral parameters for creating a
// block-based order (the single create primitive — see Backend.CreateOrder).
// It is the former StagedOrderRequest plus a Complete field: an arbitrary block
// list plus the lifecycle knob.
type CreateOrderRequest struct {
	OrderID    string // ShinGo-generated order ID (e.g. "sg-42-abc12345")
	ExternalID string // Edge UUID for correlation
	Blocks     []OrderBlock
	Priority   int
	RobotGroup string // SEER robot-dispatch group (→ rds.SetOrderRequest.Group); "" = vendor default
	// Complete marks the order finished once its blocks complete (no-wait,
	// single-shot). false leaves it staged so blocks can be appended later via
	// ReleaseOrder (multi-wait / lane-dwell orders). This field is the lane-
	// coordination (Part B) on-ramp.
	Complete bool
}

// TransportOrderResult contains the result of a successful order creation.
type TransportOrderResult struct {
	VendorOrderID string // The ID assigned by the vendor system
}

// OrderBlock describes a single block in an RDS order.
type OrderBlock struct {
	BlockID  string
	Location string
	BinTask  string // e.g., "JackLoad", "JackUnload" for SEER RDS
}
