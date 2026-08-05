package dispatch

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
)

// extendLaneLockForComplexParent is called by AdvanceCompoundOrder's terminal
// block in expose mode. The lane is ALREADY held by the complex parent — intake
// took the hold keyed by the complex parent's order id, and the compound parent
// IS the complex parent, they share an order row. The default path would release
// it here; this function's whole job is to NOT do that, so the lane stays held
// across the resume → re-resolve → pickup gap where the target bin would
// otherwise be re-buried.
//
// It no longer registers anything. The listener is the pending_lane_extensions
// row, written at intake, and it is already there — so all that remains is the
// decision not to release, plus the sanity check that the hold is really this
// parent's.
func (d *Dispatcher) extendLaneLockForComplexParent(complexParent *orders.Order, laneID, targetBinID, expectedFromNode int64) {
	if d.laneLock == nil {
		return
	}
	// Defensive against a future path that releases or transfers the hold between
	// intake and compound terminal. If it is not ours there is nothing to extend
	// and nothing to release.
	if held := d.laneLock.LockedBy(laneID); held != complexParent.ID {
		log.Printf("dispatch: lane %d not held by complex parent %d (held by %d); skipping lane-lock extension",
			laneID, complexParent.ID, held)
		return
	}
	d.dbg("complex: lane lock extended through pickup for complex parent %d (lane %d, target bin %d, expected from-node %d)",
		complexParent.ID, laneID, targetBinID, expectedFromNode)
}

// HandleBinTransitForLaneLock is called by engine wiring on
// EventBinEnteredTransit. If a lane-lock-extension listener is waiting on this
// bin AND on this from-node, the parent has picked up: release the lane.
//
// Both conditions in one statement (ConsumePendingLaneExtensionByBin), so the
// match and the claim cannot come apart. The peek-then-consume this replaces had
// a window between them wide enough for its own comment to mention.
func (d *Dispatcher) HandleBinTransitForLaneLock(binID, fromNodeID int64) {
	if d.laneLock == nil || d.db == nil {
		return
	}
	entry, err := d.db.ConsumePendingLaneExtensionByBin(binID, fromNodeID)
	if err != nil {
		log.Printf("dispatch: consume pending_lane_extension for bin %d: %v", binID, err)
		return
	}
	if entry == nil {
		return // not a watched bin, or it moved from somewhere else
	}
	// The DELETE is the synchronization point: exactly one caller gets the row
	// back, and that caller is the unambiguous owner of the hold it names.
	d.laneLock.Unlock(entry.LaneID, entry.ComplexParentID)
	d.dbg("complex: lane lock released for complex parent %d after pickup (lane %d)",
		entry.ComplexParentID, entry.LaneID)
}

// HandleComplexParentTerminalForLaneLock is called on the parent's terminal
// events. A parent that will never pick up must not leave its lane held.
//
// The claim IS the cleanup now — one DELETE ... RETURNING both removes the
// listener and tells us whether there was a lane to release. The old version
// needed a second, defensive delete for the case where the in-memory side had
// been consumed but the row had not; with one place to look, that case does not
// exist.
func (d *Dispatcher) HandleComplexParentTerminalForLaneLock(complexParentID int64) {
	if d.laneLock == nil || d.db == nil {
		return
	}
	entry, err := d.db.ConsumePendingLaneExtensionByComplexParent(complexParentID)
	if err != nil {
		log.Printf("dispatch: consume pending_lane_extension on parent terminal for complex %d: %v", complexParentID, err)
		return
	}
	if entry == nil {
		return // never had a pending extension, or it already fired
	}
	d.laneLock.Unlock(entry.LaneID, entry.ComplexParentID)
	d.dbg("complex: lane lock released for cancelled/failed complex parent %d (lane %d)",
		complexParentID, entry.LaneID)
}

// RecoverPendingLaneExtensions runs at Core boot and PRUNES listener rows that
// can never fire — those whose complex parent is gone or already terminal.
//
// It used to re-register an in-memory listener from each row. There is no
// in-memory listener any more: the row IS the listener, and it survived the
// restart by being a row. So boot has nothing to restore, exactly as it had
// nothing to restore once the lane hold itself became a row (RestoreLaneHolds,
// deleted for the same reason).
//
// What remains is real and is why this did not disappear entirely. The terminal
// handler consumes a row when a parent is cancelled or fails; a crash between
// those two moments leaves a row whose parent will never pick up, and nothing
// else would ever clear it — the lane would stay held forever. This is that
// sweep, and only that.
func (d *Dispatcher) RecoverPendingLaneExtensions() error {
	if d.db == nil {
		return nil
	}
	rows, err := d.db.ListPendingLaneExtensions()
	if err != nil {
		return fmt.Errorf("list pending_lane_extensions: %w", err)
	}
	for _, row := range rows {
		parent, err := d.db.GetOrder(row.ComplexParentID)
		switch {
		case err != nil || parent == nil:
			log.Printf("dispatch: pending_lane_extension %d: complex parent %d missing; pruning", row.ID, row.ComplexParentID)
		case protocol.IsTerminal(parent.Status):
			log.Printf("dispatch: pending_lane_extension %d: complex parent %d already terminal (%s); pruning",
				row.ID, parent.ID, parent.Status)
		default:
			continue // live parent: the row stands, and it is the listener
		}
		// Prune through the consuming delete so the lane is released too — a
		// terminal parent that still holds a lane is the failure this sweep is
		// for, and deleting the row without releasing would strand the lane with
		// nothing left pointing at it.
		d.HandleComplexParentTerminalForLaneLock(row.ComplexParentID)
	}
	return nil
}

// planUsedExposeMode reports whether the compound's child orders
// match the expose-mode shape (no "retrieve" step). The two complex-
// parent planners differ in step list emission:
//
//   - PlanReshuffleUnburyOnly (expose mode) — only "unbury" steps.
//   - PlanReshuffleToTarget (target-node mode) — "unbury" steps + one
//     "retrieve" step.
//
// CreateCompoundChildrenOnly tags each child's PayloadDesc as
// "reshuffle <stepType>: bin N" so we can detect by prefix without
// re-parsing the plan.
func planUsedExposeMode(children []*orders.Order) bool {
	for _, c := range children {
		if len(c.PayloadDesc) >= len("reshuffle retrieve") &&
			c.PayloadDesc[:len("reshuffle retrieve")] == "reshuffle retrieve" {
			return false
		}
	}
	return true
}
