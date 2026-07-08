package dispatch

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/orders"
)

// reshuffleFailDetail is shared between the parent's status update and the
// EmitOrderFailed event payload so they can't drift. Used in
// AdvanceCompoundOrder's hasFailed branch when one or more child orders
// failed and the parent must be marked failed.
const reshuffleFailDetail = "reshuffle failed: child order failed"

// CreateCompoundOrder creates a parent order with child orders for a reshuffle plan.
// All children and bin claims are created in a single transaction. The parent
// is transitioned into StatusReshuffling via lifecycle.BeginReshuffle, so the
// caller must pass a parent in a status that has Reshuffling as a legal next
// state (Pending, Sourcing, Queued). Synthetic restore parents that already
// hold StatusReshuffling at creation use CreateCompoundChildrenOnly instead.
func (d *Dispatcher) CreateCompoundOrder(parentOrder *orders.Order, plan *ReshufflePlan) error {
	if err := d.lifecycle.BeginReshuffle(parentOrder,
		fmt.Sprintf("reshuffling: %d steps to unbury bin %d", len(plan.Steps), plan.TargetBin.ID)); err != nil {
		log.Printf("dispatch: begin reshuffle order %d: %v", parentOrder.ID, err)
	}
	return d.CreateCompoundChildrenOnly(parentOrder, plan)
}

// CreateCompoundChildrenOnly creates the compound's child orders and
// advances the first one — same as CreateCompoundOrder MINUS the
// lifecycle.BeginReshuffle call. Used by the synthetic restore-blockers
// parent, which is written directly at StatusReshuffling via a
// MarkReshuffling-style initial write and would log a spurious
// "illegal transition: reshuffling → reshuffling" warning every time
// CreateCompoundOrder's BeginReshuffle fired on an already-Reshuffling
// parent.
//
// The split keeps CreateCompoundOrder's call sites unchanged
// (simple-retrieve and complex-intake parents legitimately need the
// transition) and gives the restore path a method whose name reads
// as "wire up the children, parent is already in the right state."
func (d *Dispatcher) CreateCompoundChildrenOnly(parentOrder *orders.Order, plan *ReshufflePlan) error {
	var children []store.CompoundChild
	for _, step := range plan.Steps {
		child := &orders.Order{
			EdgeUUID:      fmt.Sprintf("%s-step-%d", parentOrder.EdgeUUID, step.Sequence),
			StationID:     parentOrder.StationID,
			OrderType:     OrderTypeMove,
			Status:        StatusPending,
			ParentOrderID: &parentOrder.ID,
			Sequence:      step.Sequence,
			PayloadDesc:   fmt.Sprintf("reshuffle %s: bin %d", step.StepType, step.BinID),
			BinID:         &step.BinID,
		}

		if step.FromNode != nil {
			child.SourceNode = step.FromNode.Name
		}
		if step.ToNode != nil {
			child.DeliveryNode = step.ToNode.Name
		}
		// Simple-retrieve reshuffles emit a "retrieve" step with no
		// ToNode and rely on this fallback to land the bin at the
		// parent retrieve's lineside DeliveryNode.
		//
		// Complex-order reshuffles do NOT go through this branch:
		//   - Expose mode (PlanReshuffleUnburyOnly) emits no retrieve
		//     step at all — the complex parent resumes and runs its
		//     own pickup against the now-accessible original slot.
		//   - Target-node mode (PlanReshuffleToTarget) sets ToNode
		//     explicitly so DeliveryNode is non-empty when we reach
		//     this check and the fallback never fires.
		//
		// Adding a "retrieve" step to PlanReshuffleUnburyOnly without
		// a ToNode would silently inherit parentOrder.DeliveryNode,
		// which for a complex parent is the *last* step's node
		// (extractEndpoints in complex_steps.go) — wrong destination
		// for the unbury step's deliverable. Don't.
		if step.StepType == protocol.StepRetrieve && child.DeliveryNode == "" {
			child.DeliveryNode = parentOrder.DeliveryNode
		}

		children = append(children, store.CompoundChild{Order: child, BinID: step.BinID})
	}

	if err := d.db.CreateCompoundChildren(children); err != nil {
		return fmt.Errorf("create compound children: %w", err)
	}

	// Start executing the first child
	return d.AdvanceCompoundOrder(parentOrder.ID)
}

// AdvanceCompoundOrder dispatches the next pending child order in a compound sequence.
func (d *Dispatcher) AdvanceCompoundOrder(parentOrderID int64) error {
	// Sibling-in-flight guard: never dispatch the next child while another
	// sibling is non-pending and non-terminal (sourcing / acknowledged /
	// dispatched / in_transit / staged / delivered / faulted). Without this,
	// fireCompleted firing on BOTH (*, Delivered) and (Delivered, Confirmed)
	// would advance the next child twice across one sibling's lifecycle —
	// once before the bin lands and again after edge confirm — and the
	// redundant createCompound→advanceCompound path used to fire a second
	// dispatch within milliseconds of compound creation. On 2026-05-27 these
	// stacked to dispatch three robots into the same cross-aisle corridor.
	//
	// This guard is sequential-only — it inspects state at call time without
	// holding a lock. A pg_advisory_lock(parentOrderID) at top/bottom would
	// close the two-goroutine race; deferred until the audit query (see
	// SHINGO_TODO "Reshuffle dispatch cascade") shows it has ever fired.
	children, _ := d.db.ListChildOrders(parentOrderID)
	for _, c := range children {
		if c.Status != StatusPending && !protocol.IsTerminal(c.Status) {
			return nil
		}
	}

	next, err := d.db.GetNextChildOrder(parentOrderID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// A real DB error (connection drop, query/scan failure) is NOT the same
		// as "no more pending children". Bail without transitioning the parent or
		// releasing the lane so the next completion/failure event retries —
		// instead of prematurely completing/failing/resuming the parent (and
		// unlocking the lane) while child reshuffle steps are still queued, the
		// 2026-05-27 three-robots-in-one-corridor failure class.
		log.Printf("dispatch: get next child for compound %d: %v", parentOrderID, err)
		return err
	}
	if err != nil {
		// sql.ErrNoRows — no more PENDING children. But "not pending" doesn't mean "done".
		// Children that are dispatched / in_transit / staged / delivered are
		// in flight. We only confirm or fail the compound parent when every
		// child has reached a terminal status (confirmed / failed / cancelled).
		// Without this check, redundant child-completion events (sim FINISHED
		// + HandleOrderReceipt firing back-to-back) can advance through the
		// pending children fast enough that this branch runs before any
		// child has actually confirmed — and CompleteCompound then races
		// ahead of the still-in-flight legs, leaving the lifecycle gate to
		// reject a later child failure with `confirmed -> failed`.
		children, listErr := d.db.ListChildOrders(parentOrderID)
		if listErr != nil {
			log.Printf("dispatch: list children for compound %d: %v", parentOrderID, listErr)
		}
		hasFailed := false
		allTerminal := true
		for _, c := range children {
			if c.Status == StatusFailed {
				hasFailed = true
			}
			if !protocol.IsTerminal(c.Status) {
				allTerminal = false
			}
		}

		// Load parent for both branches.
		parent, pErr := d.db.GetOrder(parentOrderID)
		if pErr != nil {
			log.Printf("dispatch: load parent compound order %d: %v", parentOrderID, pErr)
		}

		if hasFailed {
			log.Printf("dispatch: compound order %d has failed children — marking parent failed", parentOrderID)
			if parent != nil {
				if err := d.lifecycle.Fail(parent, parent.StationID, "reshuffle_failed", reshuffleFailDetail); err != nil {
					log.Printf("dispatch: fail compound order %d: %v", parentOrderID, err)
				}
			}
			d.unlockLaneForCompound(parentOrderID)
			return nil
		}

		// In-flight children remain. Wait for the next real completion or
		// failure event to call us back. CompleteCompound below would
		// otherwise transition the parent to Confirmed prematurely.
		if !allTerminal {
			return nil
		}

		// All children reached a terminal status with none failed -> compound
		// order is complete. Route on whether the parent has its OWN work to
		// resume after the reshuffle — Stage 4 keys this on the coordinated-plan
		// signal (IsCoordinated == parent carries a step plan), not OrderType:
		//   - coordinated parent (a complex order carries StepsJSON): a buried-bin
		//     reshuffle whose parent still owes its original pickup. ResumeCompound
		//     transitions Reshuffling → Queued so the scanner re-resolves that
		//     pickup against the now-accessible slot. Do NOT call CompleteOrder —
		//     the parent hasn't finished, it's resuming.
		//   - plain parent (simple-retrieve compounds, restock compounds for the
		//     dual-mode reshuffle — no step plan): CompleteCompound transitions
		//     Reshuffling → Confirmed and fires fireCompleted.
		//
		// Sequencing dependency on fulfillment.RunOnce being synchronous
		// — see lifecycle.go's {Reshuffling, Queued} actionMap entry.
		//
		// Lane-lock handling (v7 Step 4.5):
		//   - target-node mode (PlanReshuffleToTarget): unlock immediately
		//     — the target bin has already moved out of the lane, no
		//     re-burial risk.
		//   - expose mode (PlanReshuffleUnburyOnly): TRANSFER the lock
		//     from the compound parent to the complex parent, register a
		//     listener that releases on EventBinEnteredTransit for the
		//     target bin or on parent cancel/fail. Closes the
		//     post-compound / pre-pickup re-burial window.
		//   - non-complex parents (simple-retrieve, restore): unlock
		//     immediately (existing behavior).
		if parent != nil {
			if IsCoordinated(parent) {
				if err := d.lifecycle.ResumeCompound(parent); err != nil {
					log.Printf("dispatch: resume compound order %d: %v", parentOrderID, err)
				}
			} else {
				if err := d.lifecycle.CompleteCompound(parent); err != nil {
					log.Printf("dispatch: confirm compound order %d: %v", parentOrderID, err)
				}
				if err := d.db.CompleteOrder(parentOrderID); err != nil {
					log.Printf("dispatch: complete compound order %d: %v", parentOrderID, err)
				}
			}
		}

		if parent != nil && IsCoordinated(parent) && planUsedExposeMode(children) {
			d.extendLaneLockForExposeMode(parentOrderID, parent, children)
		} else {
			d.unlockLaneForCompound(parentOrderID)
		}
		return nil
	}

	// Dispatch the child to fleet
	if next.SourceNode == "" || next.DeliveryNode == "" {
		if err := d.db.FailOrderAtomic(next.ID, "missing source or delivery node"); err != nil {
			log.Printf("dispatch: atomic fail child order %d: %v", next.ID, err)
		}
		return d.AdvanceCompoundOrder(parentOrderID)
	}

	sourceNode, err := d.db.GetNodeByDotName(next.SourceNode)
	if err != nil {
		if dbErr := d.db.FailOrderAtomic(next.ID, fmt.Sprintf("source node %q not found", next.SourceNode)); dbErr != nil {
			log.Printf("dispatch: atomic fail child order %d: %v", next.ID, dbErr)
		}
		return d.AdvanceCompoundOrder(parentOrderID)
	}

	destNode, err := d.db.GetNodeByDotName(next.DeliveryNode)
	if err != nil {
		if dbErr := d.db.FailOrderAtomic(next.ID, fmt.Sprintf("delivery node %q not found", next.DeliveryNode)); dbErr != nil {
			log.Printf("dispatch: atomic fail child order %d: %v", next.ID, dbErr)
		}
		return d.AdvanceCompoundOrder(parentOrderID)
	}

	if err = d.lifecycle.MoveToSourcing(next, "dispatcher", "dispatching reshuffle step"); err != nil {
		log.Printf("dispatch: child order %d → sourcing: %v", next.ID, err)
	}
	log.Printf("dispatch: advancing compound order %d, step %d (seq %d)", parentOrderID, next.ID, next.Sequence)

	// Build a synthetic envelope for the child dispatch
	env := d.syntheticEnvelope(next.StationID)
	d.dispatchToFleet(next, env, sourceNode, destNode)
	return nil
}

// HandleChildOrderComplete is called when a child order completes.
func (d *Dispatcher) HandleChildOrderComplete(childOrder *orders.Order) {
	if childOrder.ParentOrderID == nil {
		return
	}
	d.AdvanceCompoundOrder(*childOrder.ParentOrderID)
}

// HandleChildOrderFailure handles failure of a child in a compound order.
// Cancels ALL remaining non-terminal children (including in-flight ones)
// and fails the parent. Uses lifecycle.CancelOrder to ensure fleet orders
// are cancelled and bins are unclaimed — same approach as cancelCompoundChildren.
func (d *Dispatcher) HandleChildOrderFailure(parentOrderID, childOrderID int64) {
	log.Printf("dispatch: child order %d failed in compound %d, cancelling remaining", childOrderID, parentOrderID)

	// This handler fires once (engine wiring, on the failure event) with no
	// retry, so an early return on a transient DB error must not leave the lane
	// locked forever. Release it on every path — unlockLaneForCompound falls
	// back to an owner-based release when the children can't be read.
	defer d.unlockLaneForCompound(parentOrderID)

	// Cancel remaining non-terminal children (including in-flight)
	children, err := d.db.ListChildOrders(parentOrderID)
	if err != nil {
		log.Printf("dispatch: list children for failed compound %d: %v", parentOrderID, err)
		return
	}

	parent, err := d.db.GetOrder(parentOrderID)
	if err != nil {
		log.Printf("dispatch: load parent for failed compound %d: %v", parentOrderID, err)
		return
	}

	cancelReason := fmt.Sprintf("sibling order %d failed during reshuffle", childOrderID)
	for _, child := range children {
		if child.ID == childOrderID {
			continue
		}
		if protocol.IsTerminal(child.Status) {
			continue
		}
		d.lifecycle.CancelOrder(child, parent.StationID, cancelReason)
	}

	// Fail the parent — Fail handles the atomic transition + emit.
	if err := d.lifecycle.Fail(parent, parent.StationID, "reshuffle_failed",
		fmt.Sprintf("child order %d failed during reshuffle", childOrderID)); err != nil {
		log.Printf("dispatch: fail compound parent %d: %v", parentOrderID, err)
	}
}

// cancelCompoundChildren cancels all non-terminal children of a compound order.
// Unlike HandleChildOrderFailure (which only cancels pending/sourcing children),
// this method also cancels in-flight children (dispatched, in_transit, staged)
// and their fleet orders. Called when an operator cancels a compound parent directly.
func (d *Dispatcher) cancelCompoundChildren(parent *orders.Order, stationID, reason string) {
	children, err := d.db.ListChildOrders(parent.ID)
	if err != nil {
		log.Printf("dispatch: cancel compound children for order %d: %v", parent.ID, err)
		return
	}

	cancelReason := fmt.Sprintf("parent order cancelled: %s", reason)
	for _, child := range children {
		if protocol.IsTerminal(child.Status) {
			continue
		}
		d.lifecycle.CancelOrder(child, stationID, cancelReason)
	}

	d.unlockLaneForCompound(parent.ID)
}

// extendLaneLockForExposeMode runs in AdvanceCompoundOrder's terminal
// block when an expose-mode compound for a complex parent finishes.
// The lane lock is already held by the complex parent (the intake
// handler took the lock keyed by the parent's order ID); we arm a
// listener via d.laneHolds that releases the lock on
// EventBinEnteredTransit for the target bin. The target bin ID was
// persisted to pending_lane_extensions at intake time (the row we
// look up here) — the v7-era derivation by walking the lane and
// excluding blockers was replaced post-v7 because it coupled the
// listener to a contextual invariant (lane-locked-so-no-other-bins)
// that a future lane-lock refactor could silently break.
//
// On any failure to find the persisted row, fall back to the
// unconditional unlock — safer to release than to leave a stuck
// lane. The missing row indicates either (a) the intake-time
// persist failed (logged at the call site), or (b) something else
// already consumed the row.
func (d *Dispatcher) extendLaneLockForExposeMode(_ int64, complexParent *orders.Order, _ []*orders.Order) {
	if d.laneLock == nil || d.laneHolds == nil {
		d.unlockLaneForCompound(complexParent.ID)
		return
	}
	row, err := d.db.GetPendingLaneExtensionByComplexParent(complexParent.ID)
	if err != nil || row == nil {
		log.Printf("dispatch: extendLaneLockForExposeMode: no pending_lane_extension for complex %d (%v); releasing lock unconditionally",
			complexParent.ID, err)
		d.unlockLaneForCompound(complexParent.ID)
		return
	}
	d.extendLaneLockForComplexParent(complexParent, row.LaneID, row.TargetBinID, row.ExpectedFromNodeID)
}

// unlockLaneForCompound finds and unlocks the lane associated with a compound order's children.
func (d *Dispatcher) unlockLaneForCompound(parentOrderID int64) {
	if d.laneLock == nil {
		return
	}
	children, err := d.db.ListChildOrders(parentOrderID)
	if err == nil {
		for _, child := range children {
			if child.SourceNode != "" {
				sourceNode, err := d.db.GetNodeByDotName(child.SourceNode)
				if err == nil && sourceNode.ParentID != nil {
					d.laneLock.Unlock(*sourceNode.ParentID)
					return
				}
			}
		}
	}
	// Could not resolve the lane from children (DB error, no children, or no
	// child carries a source node). Release by owning order so a failed or
	// unreadable compound can't strand the lane lock forever.
	d.laneLock.UnlockByOwner(parentOrderID)
}
