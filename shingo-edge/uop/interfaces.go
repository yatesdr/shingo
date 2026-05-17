// interfaces.go — segregated interfaces over the Mutator's verb surface.
//
// Phase 3b organisation: split the engine's view of the mutator into
// narrow sub-interfaces by plant-event concern. Engine functions can
// depend on the slice they actually use rather than the full 16-method
// surface.
//
// The concrete *Mutator satisfies all sub-interfaces; the composition
// root wires it as the umbrella `Sink` (see engine.InventoryDeltaSink).
// Test fakes can satisfy a single sub-interface when they only need to
// exercise one concern.
package uop

import "shingo/protocol"

// Ticker — PLC tick path delta emission. Paired bucket drain + bin
// emission with reason taxonomy locked at the verb boundary.
type Ticker interface {
	Consumed(ev TickEvent) error
	Produced(ev TickEvent) error
	Fallthrough(ev TickEvent) error
}

// SlotWriter — runtime-row mutations on process_node_runtime_states.
// Each verb maps 1:1 to an underlying store call so gap-window
// semantics are visible at the call site.
type SlotWriter interface {
	BindActiveBin(nodeID, binID int64) error
	ClearActiveBin(nodeID int64) error
	PrepareIncoming(nodeID int64, incomingBinID int64, uop int) error
	ClearCache(nodeID int64) error
	SetClaimAndCount(nodeID int64, activeClaimID *int64, uop int) error
	ClearActiveAndReset(nodeID int64, activeClaimID *int64) error
	OnDelivered(nodeID int64, activeClaimID *int64, binID int64, uop int) error
	ManualLoad(nodeID int64, activeClaimID *int64, binID *int64, uop int) error
}

// Capturer — operator release-click capture (atomic bin + bucket
// emissions) and admin bucket adjustment.
type Capturer interface {
	CaptureToLineside(ev CaptureEvent) (int, error)
	AdjustBucket(nodeID int64, pairKey string, styleID int64, partNumber string, currentQty, newQty int, reason protocol.LinesideBucketDeltaReason) error
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

// Backfiller — one-shot lineside bucket seeding for fresh Core deployments.
type Backfiller interface {
	Backfill(force bool) (int, error)
}

// Sink composes every sub-interface plus the legacy four-method shim
// (RecordBin/RecordBucket/Flush/FlushFailures). This is the umbrella
// the engine holds; sub-interfaces are for finer-grained depend-on
// surfaces at call sites or in test fakes.
//
// engine.InventoryDeltaSink aliases this type so existing engine code
// continues to reference `engine.InventoryDeltaSink` and any new
// engine code can prefer the narrower sub-interfaces.
type Sink interface {
	Ticker
	SlotWriter
	Capturer
	Pickup
	Boundary
	Backfiller

	// Legacy four-method surface — carried for backward compat with
	// the original InventoryDeltaSink contract. New emission sites
	// should not call these; the archtest enforces no direct
	// RecordBin/RecordBucket calls outside this package.
	RecordBin(binID int64, payloadCode string, delta int, reason protocol.BinUOPDeltaReason)
	RecordBucket(nodeID int64, pairKey string, styleID int64, partNumber, payloadCode string, delta int, reason protocol.LinesideBucketDeltaReason)
	Flush()
	FlushFailures() int64
}
