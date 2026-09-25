// store_iface.go — narrow interfaces uop/ depends on.
//
// The Mutator never imports shingoedge/engine or any service package.
// It depends on these small interfaces, which *store.DB satisfies at
// composition time. The narrow shape keeps uop/ self-contained, makes
// test fakes feasible, and means swapping persistence replaces one
// implementation without touching any verb internals.
//
//   - runtimeWriter: process_node_runtime_states pointer/count writes, one
//     per slot-lifecycle verb.
//   - bucketStore: the lineside pile writes and reads the capture and the
//     boot resend need.
package uop

import "shingoedge/store/lineside"

// runtimeWriter is the write surface on process_node_runtime_states
// that uop verbs need. *store.DB satisfies this; engine wires the
// dependency through at construction so uop/ never imports the store.
type runtimeWriter interface {
	// ClearProcessNodeActiveBinAndCount clears the bin pointer and zeroes
	// the cached count in one statement. Used by ClearActiveBin when the
	// bin physically departs the slot (the count on the row is the
	// departed bin's).
	ClearProcessNodeActiveBinAndCount(processNodeID int64) error

	// SetProcessNodeActiveBinIDAndEpoch writes active_bin_id and
	// active_bin_epoch together. Used by BindActiveBin when the epoch
	// is known (loader L1 confirm with Core's LoadBin response).
	SetProcessNodeActiveBinIDAndEpoch(processNodeID int64, activeBinID *int64, deltaEpoch int64) error

	// SetProcessNodeRuntimeWithBin writes active_claim_id, active_bin_id,
	// and remaining_uop_cached atomically. Used by ClearActiveAndReset
	// (Order B completion at supermarket — claim preserved, active_bin
	// nulled, count zeroed).
	SetProcessNodeRuntimeWithBin(processNodeID int64, activeClaimID, activeBinID *int64, remainingUOP int) error

	// SetProcessNodeRuntimeWithBinAndEpoch writes active_claim_id,
	// active_bin_id, active_bin_epoch, and remaining_uop_cached
	// atomically. Used by ManualLoad when the epoch is known (operator
	// imprint via Core's LoadBin response).
	SetProcessNodeRuntimeWithBinAndEpoch(processNodeID int64, activeClaimID, activeBinID *int64, deltaEpoch int64, remainingUOP int) error

	// SetProcessNodeRuntime writes active_claim_id + remaining_uop_cached
	// without touching either bin pointer. Used by SetClaimAndCount —
	// the ClearBin manual-swap path, changeover switch-node, changeover
	// restore safety net, and the manual/drop Order B completion's
	// count carry-forward all share this shape.
	SetProcessNodeRuntime(processNodeID int64, activeClaimID *int64, remainingUOP int) error

	// SetProcessNodeRuntimeClaimCountAndEpoch writes claim + count and
	// advances the stamp for the carrier it names, without touching
	// either bin pointer. Used by SetClaimCountAndEpoch — the clear
	// routes, where Core starts the carrier's next life and hands back
	// the new stamp, but the carrier itself has not moved.
	SetProcessNodeRuntimeClaimCountAndEpoch(processNodeID int64, activeClaimID *int64, remainingUOP int, binID, deltaEpoch int64) error

	// SetProcessNodeRuntimeForDeliveredBin atomically writes
	// active_claim_id, active_bin_id, active_bin_epoch, and
	// remaining_uop_cached when a bin physically arrives at the slot.
	// Used by OnDelivered — the count and epoch are seeded from the
	// OrderDelivered envelope.
	SetProcessNodeRuntimeForDeliveredBin(processNodeID int64, activeClaimID *int64, binID int64, deltaEpoch int64, remainingUOP int) error
}

// bucketStore is the lineside pile surface uop verbs need.
type bucketStore interface {
	// CaptureLinesideBucket adds qty to the node's active pile of the
	// payload (creating it) and returns the pile's new qty. Never touches a
	// stranded row. Used by CaptureToLineside.
	CaptureLinesideBucket(nodeID int64, payloadCode string, qty int) (int, error)

	// ListLinesidePileKeys returns the key of every pile row. Used by
	// ResendLevels at boot.
	ListLinesidePileKeys() ([]lineside.Key, error)
}
