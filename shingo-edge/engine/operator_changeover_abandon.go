package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
)

// ErrPartnerInFlight is domain.ErrPartnerInFlight, kept here for the callers
// that test for it by this name.
var ErrPartnerInFlight = domain.ErrPartnerInFlight

// AbandonChangeoverSupply is the operator exit from awaiting_material — the
// C(ii) park where a changeover supply order sits queued because the material
// pool at its node is dry. Two shapes, chosen by acceptHalf:
//
//   - plain abandon (acceptHalf=false): the supply is cancelled with an
//     ordinary reason, so Core's swap-peer arm cascades the cancel onto the
//     partner evac fail-closed. Refused with ErrPartnerInFlight while the evac
//     is with the fleet, faulted included (domain.AbandonRefusal). Edge
//     cancels ONLY the supply — the evac's cancel arrives back as Core's
//     push, keeping a single cancel writer.
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
	if task.NextMaterialOrderID == nil {
		return domain.AbandonRefusal(*task, nil, nil, acceptHalf)
	}
	supply, err := e.db.GetOrder(*task.NextMaterialOrderID)
	if err != nil {
		return fmt.Errorf("get supply order %d: %w", *task.NextMaterialOrderID, err)
	}
	var evac *domain.Order
	if evacID := task.OldMaterialReleaseOrderID; evacID != nil {
		if o, gerr := e.db.GetOrder(*evacID); gerr == nil {
			evac = o
		}
	}
	if err := domain.AbandonRefusal(*task, supply, evac, acceptHalf); err != nil {
		return err
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
