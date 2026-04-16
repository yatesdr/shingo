package engine

import (
	"log"

	"shingoedge/orders"
	"shingoedge/store"
)

// restoreChangeoverState runs on engine startup after wireEventHandlers.
// It reconciles changeover node task states against order statuses for any
// active changeover that was in progress when Edge last shut down.
//
// Timing: StartupReconcile (order status sync with Core) runs asynchronously.
// This function runs synchronously during startup and does a best-effort
// catch-up for orders that already completed. Orders that haven't synced yet
// will be caught later by the normal wiring when the async sync completes.
func (e *Engine) restoreChangeoverState() {
	processes, err := e.db.ListProcesses()
	if err != nil {
		return
	}
	for _, process := range processes {
		changeover, err := e.db.GetActiveProcessChangeover(process.ID)
		if err != nil {
			continue // no active changeover
		}
		tasks, err := e.db.ListChangeoverNodeTasks(changeover.ID)
		if err != nil {
			continue
		}

		restored := 0
		for i := range tasks {
			if tasks[i].Situation == "unchanged" {
				continue
			}
			if e.reconcileNodeTask(&tasks[i], changeover.ToStyleID) {
				restored++
			}
		}
		if restored > 0 {
			log.Printf("changeover restore: reconciled %d node tasks for process %d changeover %d",
				restored, process.ID, changeover.ID)
		}

		// Check if changeover can be completed (all orders terminal, all nodes done)
		if err := e.tryCompleteProcessChangeover(process.ID); err != nil {
			log.Printf("changeover: complete process changeover for %d: %v", process.ID, err)
		}
	}
}

// reconcileNodeTask checks linked orders for a changeover node task and
// advances the node task state if orders completed while Edge was down.
// Returns true if any state advancement was made.
func (e *Engine) reconcileNodeTask(task *store.ChangeoverNodeTask, toStyleID int64) bool {
	advanced := false

	// Resolve CoreNodeName from process node for claim lookups.
	coreNodeName := ""
	if node, err := e.db.GetProcessNode(task.ProcessNodeID); err == nil {
		coreNodeName = node.CoreNodeName
	}

	// Check staging/delivery order (NextMaterialOrderID)
	if task.NextMaterialOrderID != nil {
		if order, err := e.db.GetOrder(*task.NextMaterialOrderID); err == nil {
			if orders.IsTerminal(order.Status) {
				switch task.State {
				case "staging_requested":
					if toStyleID > 0 && coreNodeName != "" {
						if toClaim, err := e.db.GetStyleNodeClaimByNode(toStyleID, coreNodeName); err == nil {
							claimID := toClaim.ID
							if err := e.db.SetProcessNodeRuntime(task.ProcessNodeID, &claimID, 0); err != nil {
				log.Printf("changeover: set runtime for node %d: %v", task.ProcessNodeID, err)
				}
						}
					}
					if err := e.db.UpdateChangeoverNodeTaskState(task.ID, "staged"); err != nil {
				log.Printf("changeover: update node task %d to staged: %v", task.ID, err)
				}
					advanced = true
				case "release_requested":
					if err := e.db.UpdateChangeoverNodeTaskState(task.ID, "released"); err != nil {
				log.Printf("changeover: update node task %d to released: %v", task.ID, err)
				}
					advanced = true
				}
			}
		}
	}

	// Check evacuation/clear order (OldMaterialReleaseOrderID)
	if task.OldMaterialReleaseOrderID != nil {
		if order, err := e.db.GetOrder(*task.OldMaterialReleaseOrderID); err == nil {
			if orders.IsTerminal(order.Status) && task.State == "empty_requested" {
				if err := e.db.UpdateChangeoverNodeTaskState(task.ID, "line_cleared"); err != nil {
				log.Printf("changeover: update node task %d to line_cleared: %v", task.ID, err)
				}
				advanced = true
			}
		}
	}

	return advanced
}
