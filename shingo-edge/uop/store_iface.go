// store_iface.go — narrow interfaces uop/ depends on.
//
// The Mutator never imports shingoedge/engine or any service package.
// It depends on these small interfaces, which *store.DB satisfies at
// composition time. The narrow shape keeps uop/ self-contained, makes
// test fakes feasible, and means swapping persistence (e.g. for the
// future audit rewrite) replaces one implementation without touching
// any verb internals.
//
// Phase 3a grows this file incrementally as verbs land. Today's set:
//
//   - runtimeWriter: process_node_runtime_states pointer/count writes.
//     Used by BindActiveBin, ClearActiveBin (Phase 3a wedge 2) and
//     will grow with each slot-lifecycle verb (SetClaimAndCount,
//     OnDelivered, ManualLoad, ClearActiveAndReset).
//
// Future additions (not yet added):
//   - runtimeReader: GetProcessNodeRuntime for verbs that need cache state.
//   - bucketStore: lineside bucket reads/writes for AdjustBucket / CaptureToLineside.
//   - nodeStore: process node listing for Backfill.
package uop

import (
	"shingoedge/store/lineside"
	"shingoedge/store/processes"
)

// runtimeWriter is the write surface on process_node_runtime_states
// that uop verbs need. *store.DB satisfies this; engine wires the
// dependency through at construction so uop/ never imports the store.
type runtimeWriter interface {
	// SetProcessNodeActiveBinID writes only the active bin pointer on
	// a runtime row. Used by ClearActiveBin (pickup clear). Pass nil
	// to clear the pointer.
	SetProcessNodeActiveBinID(processNodeID int64, activeBinID *int64) error

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

	// SetProcessNodeRuntimeForDeliveredBin atomically writes
	// active_claim_id, active_bin_id, active_bin_epoch, and
	// remaining_uop_cached when a bin physically arrives at the slot.
	// Used by OnDelivered — the count and epoch are seeded from the
	// OrderDelivered envelope.
	SetProcessNodeRuntimeForDeliveredBin(processNodeID int64, activeClaimID *int64, binID int64, deltaEpoch int64, remainingUOP int) error
}

// bucketStore is the read/write surface on lineside_buckets that uop
// verbs need.
type bucketStore interface {
	// SetLinesideBucketForReconcile writes the bucket's qty to an
	// exact value (not a delta). Deletes the row when qty=0; matches
	// the existing "SetForReconcile" semantic. Used by AdjustBucket.
	SetLinesideBucketForReconcile(nodeID int64, pairKey string, styleID int64, partNumber string, qty int) error

	// ListLinesideBuckets returns every bucket row for the given
	// node. Used by Backfill to enumerate the seed deltas at startup.
	ListLinesideBuckets(nodeID int64) ([]lineside.Bucket, error)

	// CaptureLinesideBucket adds qty to the bucket for the given key
	// (creating the row if absent, activating an inactive row).
	// Returns the resulting bucket row. Used by CaptureToLineside
	// during operator release-click capture.
	CaptureLinesideBucket(nodeID int64, pairKey string, styleID int64, partNumber string, qty int) (*lineside.Bucket, error)

	// DeactivateOtherLinesideStyles marks all bucket rows for this
	// node EXCEPT the one matching styleID as inactive. Fires on
	// every release click — "this style is now active here" — so
	// future drain operations resolve to the right active bucket.
	DeactivateOtherLinesideStyles(nodeID int64, styleID int64) error
}

// nodeStore is the read surface on process_nodes that uop verbs need.
type nodeStore interface {
	// ListProcessNodes returns every process_node row this Edge knows
	// about. Used by Backfill to walk each node's buckets.
	ListProcessNodes() ([]processes.Node, error)
}
