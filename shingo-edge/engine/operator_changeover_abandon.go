package engine

import (
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
)

// ErrPartnerInFlight rejects a plain abandon while the partner evacuation is
// already executing on the fleet. Cancelling the supply would cascade a cancel
// onto an evac the robot is mid-move on — the divergence family the faulted-
// release fix closed. The operator's ways out are to wait, or to accept the
// half-swap (evac completes, supply alone is cancelled).
var ErrPartnerInFlight = errors.New("partner evacuation is already executing on the fleet; wait for it to finish or accept the half-swap")

// AbandonChangeoverSupply is the operator exit from awaiting_material — the
// C(ii) park where a changeover supply order sits queued because the material
// pool at its node is dry. Two shapes, chosen by acceptHalf:
//
//   - plain abandon (acceptHalf=false): the supply is cancelled with an
//     ordinary reason, so Core's swap-peer arm cascades the cancel onto the
//     partner evac fail-closed. Refused with ErrPartnerInFlight while the evac
//     is fleet-active (see above). Edge cancels ONLY the supply — the evac's
//     cancel arrives back as Core's push, keeping a single cancel writer.
//
//   - accept half-swap (acceptHalf=true): the supply is cancelled with
//     protocol.CancelReasonAcceptHalfSwap, which Core maps to its abandoned
//     terminal kind and leaves the partner evac to complete. The line ends up
//     cleared with no new material — the half the operator chose to keep.
//
// Either way the node task lands NodeTaskAbandoned (terminal) with an
// operator-facing skip note, and the changeover completion gate re-runs.
func (e *Engine) AbandonChangeoverSupply(processID, processNodeID int64, acceptHalf bool, calledBy string) error {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return fmt.Errorf("no active changeover for process %d: %w", processID, err)
	}
	tasks, err := e.db.ListChangeoverNodeTasks(changeover.ID)
	if err != nil {
		return fmt.Errorf("list node tasks: %w", err)
	}
	var task *domain.NodeTask
	for i := range tasks {
		if tasks[i].ProcessNodeID == processNodeID {
			task = &tasks[i]
			break
		}
	}
	if task == nil {
		return fmt.Errorf("no changeover task for node %d", processNodeID)
	}
	if task.State != domain.NodeTaskAwaitingMaterial {
		return fmt.Errorf("node %s is not awaiting material (state %s)", task.NodeName, task.State)
	}
	if task.NextMaterialOrderID == nil {
		return fmt.Errorf("node %s has no supply order to abandon", task.NodeName)
	}

	supply, err := e.db.GetOrder(*task.NextMaterialOrderID)
	if err != nil {
		return fmt.Errorf("get supply order %d: %w", *task.NextMaterialOrderID, err)
	}
	// The stamp does not revert when Core un-parks the order, so re-check the
	// live status: material already in motion should ARRIVE, not be abandoned.
	if !orders.IsTerminal(supply.Status) && !protocol.IsAcquiring(supply.Status) {
		return fmt.Errorf("supply order %d is now in motion (%s) — material is on its way", supply.ID, supply.Status)
	}

	if !acceptHalf {
		if evacID := task.OldMaterialReleaseOrderID; evacID != nil {
			if evac, gerr := e.db.GetOrder(*evacID); gerr == nil && protocol.IsVendorActive(evac.Status) {
				return ErrPartnerInFlight
			}
		}
	}

	// A terminal supply (e.g. cancelled out-of-band while the task stayed
	// parked) has nothing left to cancel — the abandon then only settles the
	// task state, which is exactly the recovery such a dead-end needs.
	if !orders.IsTerminal(supply.Status) {
		reason := "changeover supply abandoned by operator"
		if acceptHalf {
			reason = protocol.CancelReasonAcceptHalfSwap
		}
		if err := e.orderMgr.AbortOrderWithReason(supply.ID, reason); err != nil {
			return fmt.Errorf("cancel supply order %d: %w", supply.ID, err)
		}
	}

	note := "supply abandoned: material unavailable at " + task.NodeName
	if acceptHalf {
		note = "half-swap accepted: line cleared, supply abandoned at " + task.NodeName
	}
	if err := e.db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskAbandoned); err != nil {
		return fmt.Errorf("stamp node task %d abandoned: %w", task.ID, err)
	}
	if err := e.db.SetChangeoverNodeTaskSkipNote(task.ID, note); err != nil {
		log.Printf("changeover abandon: set skip note on task %d: %v", task.ID, err)
	}
	log.Printf("changeover abandon: process=%d node=%s supply=%d accept_half=%t by=%s",
		processID, task.NodeName, supply.ID, acceptHalf, calledBy)

	if err := e.tryCompleteProcessChangeover(processID); err != nil {
		log.Printf("changeover: try-complete after abandon for process %d: %v", processID, err)
	}
	return nil
}
