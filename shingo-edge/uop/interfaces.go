// interfaces.go — segregated interfaces over the Mutator's verb surface.
//
// Phase 3b organisation: split the engine's view of the mutator into
// narrow sub-interfaces by plant-event concern. Engine functions can
// depend on the slice they actually use rather than the full surface.
//
// The concrete *Mutator satisfies all sub-interfaces; the composition
// root wires it as the umbrella `Sink` (see engine.InventoryDeltaSink).
// Test fakes can satisfy a single sub-interface when they only need to
// exercise one concern.
package uop

import "shingoedge/store/lineside"

// Ticker — PLC tick path emission. Paired pile drain + bin emission with
// reason taxonomy locked at the verb boundary.
type Ticker interface {
	Consumed(ev TickEvent) error
	Produced(ev TickEvent) error
	Fallthrough(ev TickEvent) error
}

// SlotWriter — runtime-row mutations on process_node_runtime_states.
// Each verb maps 1:1 to an underlying store call so the slot-lifecycle
// intent is visible at the call site.
type SlotWriter interface {
	BindActiveBin(nodeID, binID int64, deltaEpoch int64) error
	ClearActiveBin(nodeID int64) error
	SetClaimAndCount(nodeID int64, activeClaimID *int64, uop int) error
	SetClaimCountAndEpoch(nodeID int64, activeClaimID *int64, uop int, binID, deltaEpoch int64) error
	ClearActiveAndReset(nodeID int64, activeClaimID *int64) error
	OnDelivered(nodeID int64, activeClaimID *int64, binID int64, deltaEpoch int64, uop int) error
	ManualLoad(nodeID int64, activeClaimID *int64, binID *int64, deltaEpoch int64, uop int) error
}

// Capturer — operator release-click capture (the piles' gain and the bin's
// reduction, together).
type Capturer interface {
	CaptureToLineside(ev CaptureEvent) (int, error)
}

// Piles — lineside pile writes made outside the capture and the tick (the
// cutover's strand, the admin Clear, a process delete), and the boot resend.
// The caller writes the rows; these send the resulting levels.
type Piles interface {
	// PilesChanged marks each key's level dirty and flushes, so Core's
	// mirror has the new levels before the call returns to the operator.
	PilesChanged(keys ...lineside.Key)
	// ResendLevels marks every pile row's key dirty and flushes: every
	// level goes out again, unconditionally. Returns how many keys.
	ResendLevels() (int, error)
}

// Pickup — bin-pickup boundary event (flush before slot pointers clear).
type Pickup interface {
	OnBinPickedUp(nodeID *int64) error
}

// Boundary — non-UOP orchestration boundary that needs a flush before
// attribution context changes. Today's caller: A/B active-pull flip.
type Boundary interface {
	MarkAttributionBoundary(nodeID int64) error
}

// Sink composes every sub-interface plus Flush. This is the umbrella the
// engine holds; sub-interfaces are for finer-grained depend-on surfaces at
// call sites or in test fakes.
//
// engine.InventoryDeltaSink aliases this type so existing engine code
// continues to reference `engine.InventoryDeltaSink` and any new
// engine code can prefer the narrower sub-interfaces.
type Sink interface {
	// WithPending runs fn with a snapshot of the accumulator's unflushed
	// counts and sent pile levels, under the flush lock (see pending.go). The
	// lineside report reads and enqueues inside it so its counts are stated
	// as of FlushedSeq.
	WithPending(fn func(Pending) error) error

	Ticker
	SlotWriter
	Capturer
	Piles
	Pickup
	Boundary

	// Flush runs one synchronous flush pass (operator release, loader
	// confirm, and the other boundary triggers).
	Flush()
}
