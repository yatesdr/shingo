package dispatch

import (
	"fmt"
	"log"
	"sync"

	"shingo/protocol"
	"shingocore/store/orders"
)

// laneHoldEntry tracks a lane lock held by a complex parent during
// its post-compound pre-pickup window. v7 Step 4.5: in expose mode
// the compound parent transfers the lock to the complex parent on
// compound terminal so the lane stays locked across the resume → re-
// resolve → pickup gap. This entry expires when:
//
//   - EventBinEnteredTransit fires for the target bin leaving the
//     expected slot — the parent has picked up, no more re-burial
//     risk. Release the lock.
//   - EventOrderCancelled / EventOrderFailed fires for the complex
//     parent before pickup. Release the lock so the lane isn't
//     stuck locked indefinitely.
//
// Crash-volatile (same shape as restoreRegistry and LaneLock itself).
type laneHoldEntry struct {
	complexParentID  int64
	laneID           int64
	targetBinID      int64
	expectedFromNode int64
}

// laneHoldRegistry indexes laneHoldEntries by target bin ID (for the
// EventBinEnteredTransit dispatch) and by complex parent ID (for the
// EventOrderCancelled/EventOrderFailed deregistration). Both indexes
// are mutex-guarded.
type laneHoldRegistry struct {
	mu        sync.Mutex
	byBin     map[int64]*laneHoldEntry
	byComplex map[int64]int64 // complexParentID -> targetBinID
}

func newLaneHoldRegistry() *laneHoldRegistry {
	return &laneHoldRegistry{
		byBin:     make(map[int64]*laneHoldEntry),
		byComplex: make(map[int64]int64),
	}
}

// Register stores a pending hold. Returns false on duplicate.
func (r *laneHoldRegistry) Register(entry *laneHoldEntry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byComplex[entry.complexParentID]; exists {
		return false
	}
	r.byBin[entry.targetBinID] = entry
	r.byComplex[entry.complexParentID] = entry.targetBinID
	return true
}

// ConsumeByComplexParent atomically removes and returns the entry
// keyed by complex parent ID. Used by the cancel/fail handlers.
func (r *laneHoldRegistry) ConsumeByComplexParent(complexParentID int64) *laneHoldEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	binID, ok := r.byComplex[complexParentID]
	if !ok {
		return nil
	}
	entry := r.byBin[binID]
	delete(r.byBin, binID)
	delete(r.byComplex, complexParentID)
	return entry
}

// PeekByBin returns (entry, found) without consuming. Used by the
// bin-transit handler so a mismatched FromNodeID doesn't accidentally
// consume the entry.
func (r *laneHoldRegistry) PeekByBin(binID int64) (*laneHoldEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.byBin[binID]
	return entry, ok
}

// ConsumeByBin removes and returns the entry keyed by bin ID.
func (r *laneHoldRegistry) ConsumeByBin(binID int64) *laneHoldEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.byBin[binID]
	if !ok {
		return nil
	}
	delete(r.byBin, binID)
	delete(r.byComplex, entry.complexParentID)
	return entry
}

// extendLaneLockForComplexParent is called by AdvanceCompoundOrder's
// terminal block in expose mode. The lane lock is ALREADY held by
// the complex parent (handleComplexBuriedAtIntake / planBuriedReshuffle
// take the lock keyed by the complex parent's order ID before
// CreateCompoundOrder runs, and the compound parent IS the complex
// parent — they share an order row). The default code path here
// would call d.laneLock.Unlock(laneID) and release it immediately;
// instead we skip the unlock and register a listener that releases
// the lock when the complex parent's first pickup leg actually fires
// the target bin's EventBinEnteredTransit.
//
// Failure paths (parent cancel/fail before pickup) deregister the
// listener and release the lock via the dispatcher's
// HandleComplexParentTerminalForLaneLock handler.
func (d *Dispatcher) extendLaneLockForComplexParent(complexParent *orders.Order, laneID, targetBinID, expectedFromNode int64) {
	if d.laneLock == nil || d.laneHolds == nil {
		return
	}
	// Sanity: confirm the lock is actually held by this parent.
	// Defensive against a future code path that releases or
	// transfers the lock between intake and compound terminal.
	if held := d.laneLock.LockedBy(laneID); held != complexParent.ID {
		log.Printf("dispatch: lane %d not held by complex parent %d (held by %d); skipping lane-lock extension",
			laneID, complexParent.ID, held)
		return
	}
	entry := &laneHoldEntry{
		complexParentID:  complexParent.ID,
		laneID:           laneID,
		targetBinID:      targetBinID,
		expectedFromNode: expectedFromNode,
	}
	if !d.laneHolds.Register(entry) {
		// Race: already registered for this complex parent. Release
		// the lock so it doesn't sit indefinitely. The lane is held
		// by this complex parent (we verified via LockedBy above), so
		// a plain Unlock is correct — no other caller can hold it.
		d.laneLock.Unlock(laneID, complexParent.ID)
		log.Printf("dispatch: duplicate lane-hold registration for complex %d; releasing lock", complexParent.ID)
		return
	}
	// Note: the pending_lane_extensions row was written at compound-
	// creation time (handleComplexBuriedAtIntake /
	// handleComplexBuriedOnReplay), not here. This function only
	// arms the in-memory listener.
	d.dbg("complex: lane lock extended through pickup for complex parent %d (lane %d, target bin %d, expected from-node %d)",
		complexParent.ID, laneID, targetBinID, expectedFromNode)
}

// HandleBinTransitForLaneLock is called by engine wiring on
// EventBinEnteredTransit. If the bin matches a pending lane-lock-
// extension entry AND the FromNodeID matches, release the lane lock.
// Both checks must pass — a bin moved by an unrelated order shouldn't
// trigger release.
func (d *Dispatcher) HandleBinTransitForLaneLock(binID, fromNodeID int64) {
	if d.laneHolds == nil || d.laneLock == nil {
		return
	}
	entry, ok := d.laneHolds.PeekByBin(binID)
	if !ok {
		return
	}
	if entry.expectedFromNode != 0 && entry.expectedFromNode != fromNodeID {
		d.dbg("complex: lane-lock release — bin %d transited from %d, expecting %d; ignoring",
			binID, fromNodeID, entry.expectedFromNode)
		return
	}
	if d.laneHolds.ConsumeByBin(binID) == nil {
		return // raced with another consumer
	}
	// ConsumeByBin is the synchronization point: only one caller per
	// bin survives the consume, and that caller is the unambiguous
	// owner of the lock the entry references. Plain Unlock is safe.
	d.laneLock.Unlock(entry.laneID, entry.complexParentID)
	if err := d.db.DeletePendingLaneExtensionByComplexParent(entry.complexParentID); err != nil {
		log.Printf("dispatch: delete pending_lane_extension on fire for complex %d: %v", entry.complexParentID, err)
	}
	d.dbg("complex: lane lock released for complex parent %d after pickup (lane %d)",
		entry.complexParentID, entry.laneID)
}

// HandleComplexParentTerminalForLaneLock is called on
// EventOrderCancelled / EventOrderFailed. If the complex parent had
// a pending lane-hold, release it — the parent won't pick up, so
// the lock would otherwise stay held forever.
func (d *Dispatcher) HandleComplexParentTerminalForLaneLock(complexParentID int64) {
	if d.laneHolds == nil || d.laneLock == nil {
		return
	}
	entry := d.laneHolds.ConsumeByComplexParent(complexParentID)
	if entry == nil {
		// In-memory already consumed / never registered. Sweep the
		// DB row defensively — a row could survive from a prior
		// process if RecoverPendingLaneExtensions ran but the
		// in-memory side was cleaned up early.
		if err := d.db.DeletePendingLaneExtensionByComplexParent(complexParentID); err != nil {
			log.Printf("dispatch: delete pending_lane_extension on parent terminal (no-entry) for complex %d: %v", complexParentID, err)
		}
		return
	}
	// ConsumeByComplexParent is the synchronization point — only one
	// caller per complex-parent ID consumes the entry. Plain Unlock
	// is safe here for the same reason as HandleBinTransitForLaneLock.
	d.laneLock.Unlock(entry.laneID, entry.complexParentID)
	if err := d.db.DeletePendingLaneExtensionByComplexParent(complexParentID); err != nil {
		log.Printf("dispatch: delete pending_lane_extension on parent terminal for complex %d: %v", complexParentID, err)
	}
	d.dbg("complex: lane lock released for cancelled/failed complex parent %d (lane %d)",
		complexParentID, entry.laneID)
}

// RecoverPendingLaneExtensions runs at Core boot. Scans the
// pending_lane_extensions table; for each row whose complex parent
// is still in a non-terminal status, re-registers an in-memory
// listener AND re-acquires the lane lock for that parent. Rows whose
// parent is already terminal are deleted — those listeners would
// never fire.
//
// Lane-lock re-acquisition is the difference from the restore-blockers
// recovery: the in-memory LaneLock is volatile so a Core restart drops
// it. The persisted lane extension is the only durable record that
// the lane was held by THIS complex parent at restart, so we restore
// that invariant before any other reshuffle path can race on the lane.
func (d *Dispatcher) RecoverPendingLaneExtensions() error {
	if d.db == nil || d.laneHolds == nil || d.laneLock == nil {
		return nil
	}
	rows, err := d.db.ListPendingLaneExtensions()
	if err != nil {
		return fmt.Errorf("list pending_lane_extensions: %w", err)
	}
	for _, row := range rows {
		parent, err := d.db.GetOrder(row.ComplexParentID)
		if err != nil || parent == nil {
			log.Printf("dispatch: recover pending_lane_extension %d: complex parent %d missing; deleting row", row.ID, row.ComplexParentID)
			if dErr := d.db.DeletePendingLaneExtensionByComplexParent(row.ComplexParentID); dErr != nil {
				log.Printf("dispatch: delete stale pending_lane_extension for complex %d: %v", row.ComplexParentID, dErr)
			}
			continue
		}
		if protocol.IsTerminal(parent.Status) {
			log.Printf("dispatch: recover pending_lane_extension %d: complex parent %d already terminal (%s); deleting row",
				row.ID, parent.ID, parent.Status)
			if dErr := d.db.DeletePendingLaneExtensionByComplexParent(row.ComplexParentID); dErr != nil {
				log.Printf("dispatch: delete stale pending_lane_extension for complex %d: %v", row.ComplexParentID, dErr)
			}
			continue
		}
		entry := &laneHoldEntry{
			complexParentID:  row.ComplexParentID,
			laneID:           row.LaneID,
			targetBinID:      row.TargetBinID,
			expectedFromNode: row.ExpectedFromNodeID,
		}
		if !d.laneHolds.Register(entry) {
			// Already registered in this process. Don't try to re-
			// acquire the lock.
			continue
		}
		// Re-acquire the lane lock. If another reshuffle has already
		// claimed it after the restart, we lose — the listener fires
		// but Unlock will be a no-op. That's an extremely narrow window
		// (no other code can know about this complex parent's claim
		// pre-recovery), and the worst outcome is one stale listener
		// entry that gets dropped on its own bin-transit fire.
		if !d.laneLock.TryLock(row.LaneID, row.ComplexParentID) {
			log.Printf("dispatch: recover pending_lane_extension for complex %d: lane %d already locked; in-memory listener re-registered but lock not re-acquired",
				row.ComplexParentID, row.LaneID)
		}
		log.Printf("dispatch: recovered pending_lane_extension for complex %d (lane %d, target bin %d)",
			row.ComplexParentID, row.LaneID, row.TargetBinID)
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
