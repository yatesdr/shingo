package fleet

import (
	"context"
	"time"
)

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
	// KeyRoute is the create-time robot-routing hint (→ rds.SetOrderRequest.KeyRoute):
	// an ordered list of scene key points (AdvancedPoints, a.k.a. LMs) that steer
	// SEER's robot assignment at create time — e.g. approach the source via LM10,
	// leave the destination via LM11. Empty/nil = SEER auto-picks (today's behavior).
	// Create-time only (AddBlocksRequest carries no keyRoute), so no mid-order
	// rerouting. The populator (which fills this from each node's designated
	// approach/leave LMs) is a separate follow-on; for now every caller leaves it
	// nil. NOTE: an LM that does not exist or is unreachable makes SEER terminate the
	// order immediately — validation lands with the populator, not in the conduit.
	KeyRoute []string
	// Complete marks the order finished once its blocks complete (no-wait,
	// single-shot). false leaves it staged so blocks can be appended later via
	// ReleaseOrder (multi-wait / lane-dwell orders). This field is the lane-
	// coordination (Part B) on-ramp.
	Complete bool
}

// PositionGate answers whether a robot may complete a block at a location right
// now. A plant node holds EXACTLY ONE BIN, so a robot physically cannot place a
// bin onto a position that already holds someone else's — it stalls there until
// the position clears. A real fleet gets that for free from physics: the block
// simply never reports FINISHED.
//
// The simulator has no physics. Without this it completes every block on a timer,
// so it will happily "deliver" onto an occupied node — an event that cannot occur
// in a plant. Core then does the only correct thing with an impossible input
// (a completed delivery proves the slot was empty, so the conflicting record must
// be a stale ghost) and evicts a perfectly good bin. That is not a Core bug; it is
// the sim lying. See the 2026-07-13 two-robot press-swap chase.
//
// Only a PLACEMENT can be blocked. A pickup is the robot REMOVING the bin that is
// there, and a wait is it standing next to one -- neither can be obstructed by
// occupancy, and holding them deadlocks the robot against the very bin it came for.
// (Learned the hard way: ownership alone does not distinguish the two, because
// ApplyArrival clears a bin's claim when it lands, so a compound restock leg
// arrives to collect a bin that is no longer claimed by anyone.)
//
// Implemented by the engine (which owns bins and nodes) — the fleet package stays
// vendor-neutral. Real backends do not implement PositionGated and are unaffected.
type PositionGate interface {
	// CanEnterPosition reports whether vendorOrderID's robot may complete a block
	// with binTask at location now. ok=false means HOLD — retry on a later tick.
	// blockedBy is a human-readable reason for the sim log. Only placements are
	// ever held; the implementation decides what counts as one.
	CanEnterPosition(vendorOrderID, location, binTask string) (ok bool, blockedBy string)
}

// PositionGated is the optional setter a backend exposes to receive a
// PositionGate. Only the simulator implements it; the engine wires itself in via
// a type assertion, exactly as it does for DriverStarter.
type PositionGated interface {
	SetPositionGate(g PositionGate)
}

// DriverStarter is an optional interface implemented by fleet backends whose
// driver goroutine must launch AFTER the engine's event handlers are wired
// (the sim driver, which emits FINISHED events immediately, would fire into
// a dead handler before wireEventHandlers). Backends that don't need deferred
// start simply omit it; the engine checks with a type assertion.
type DriverStarter interface {
	StartDriver(ctx context.Context) error
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
