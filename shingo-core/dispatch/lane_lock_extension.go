package dispatch

import (
	"fmt"
	"log"

	"shingo/protocol"
)

// extendLaneLockForComplexParent WAS HERE AND IS DELETED, inlined into its one
// caller (compound.go extendLaneLockForExposeMode).
//
// It was named for an action it did not perform. Once the listener became the
// pending_lane_extensions row — written at intake, consumed by
// HandleBinTransitForLaneLock below — there was nothing left to arm, and the
// body was a LockedBy sanity check plus a debug line. It mutated nothing. The
// "extension" it named is the ABSENCE of a release: the caller reaches its end
// without unlocking, and that is the whole mechanism.
//
// Worth stating rather than deleting silently, because the SHAPE is the thing. A
// function named for the mechanism, sitting where the mechanism would be, is
// where the next reader stops looking — and deleting its call site would then
// read as deleting the extension itself, when the extension is a path that
// simply does not call unlock.

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
