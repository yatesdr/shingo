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

import (
	"database/sql"

	"shingoedge/store/lineside"
	"shingoedge/store/processes"
)

// runtimeWriter is the write surface on process_node_runtime_states
// that uop verbs need. *store.DB satisfies this; engine wires the
// dependency through at construction so uop/ never imports the store.
type runtimeWriter interface {
	// ClearProcessNodeActiveBinAndCount clears the bin pointer and zeroes
	// the cached count in one statement. Used by ClearActiveBin, the
	// pointer/count half of the carrier-left verb (the count on the row is
	// the departed bin's).
	ClearProcessNodeActiveBinAndCount(processNodeID int64) error

	// LeaveProcessNodeAtPickup is the same statement at the robot's pickup,
	// also stamping the lift's generation and adding the ticks after the
	// lift to the hold (Lane H, D11). Used by LeaveAtPickup.
	LeaveProcessNodeAtPickup(processNodeID, pickupEpoch int64, hold int, holdPart string) error

	// SetProcessNodeActiveBinIDAndEpoch writes active_bin_id and
	// active_bin_epoch together. Used by BindActiveBin when the epoch
	// is known (loader L1 confirm with Core's LoadBin response).
	SetProcessNodeActiveBinIDAndEpoch(processNodeID int64, activeBinID *int64, deltaEpoch int64) error

	// SetProcessNodeRuntimeWithBinAndEpoch writes active_claim_id,
	// active_bin_id, active_bin_epoch, and remaining_uop_cached
	// atomically. Used by ManualLoad (operator imprint via Core's LoadBin
	// response) and BindFromCore (Core's announcement of the carrier at
	// the slot).
	SetProcessNodeRuntimeWithBinAndEpoch(processNodeID int64, activeClaimID, activeBinID *int64, deltaEpoch int64, remainingUOP int) error

	// SetProcessNodeActiveClaim writes active_claim_id and nothing else.
	// Used by SetClaim — the changeover's claim advances (staged delivery,
	// the release's completion, switch-node, the restore safety net).
	SetProcessNodeActiveClaim(processNodeID int64, activeClaimID *int64) error

	// SetProcessNodeRuntimeClaimCountAndEpoch writes claim + count and
	// advances the stamp for the carrier it names, without touching
	// either bin pointer. Used by SetClaimCountAndEpoch — the clear
	// routes, where Core starts the carrier's next life and hands back
	// the new stamp, but the carrier itself has not moved.
	SetProcessNodeRuntimeClaimCountAndEpoch(processNodeID int64, activeClaimID *int64, remainingUOP int, binID, deltaEpoch int64) error

	// BindEmptySlotUnlessDeparted binds a carrier into a slot read as
	// empty, refusing in its own WHERE clause when it is the carrier that
	// last left the slot at an older stamp. Used by BindStagedUnlessDeparted.
	BindEmptySlotUnlessDeparted(processNodeID int64, activeClaimID *int64, binID, deltaEpoch int64, remainingUOP int) (bool, error)

	// BindLandingAt is the landing's bind (Lane H, H.4): its refusals are
	// its WHERE. Reports whether it landed. Used by Land.
	BindLandingAt(processNodeID int64, l processes.LandingBind) (bool, error)
}

// bucketStore is the lineside pile surface uop verbs need.
type bucketStore interface {
	// CaptureLinesideBucket adds qty to the node's active pile of the
	// payload (creating it) and returns the pile's new qty. Never touches a
	// stranded row. Used by CaptureToLineside.
	CaptureLinesideBucket(nodeID int64, payloadCode string, qty int) (int, error)

	// CaptureLinesideBuckets adds every part's qty to its active pile and
	// runs inTx, in one transaction. Used by CaptureToLineside when the
	// caller records the capture (CaptureEvent.InTx).
	CaptureLinesideBuckets(nodeID int64, parts map[string]int, inTx func(tx *sql.Tx) error) error

	// ListLinesidePileKeys returns the key of every pile row. Used by
	// ResendLevels at boot.
	ListLinesidePileKeys() ([]lineside.Key, error)
}
