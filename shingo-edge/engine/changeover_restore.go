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
		for _, task := range tasks {
			if task.Situation == "unchanged" {
				continue
			}
			if e.reconcileNodeTask(&task) {
				restored++
			}
		}
		if restored > 0 {
			log.Printf("changeover restore: reconciled %d node tasks for process %d changeover %d",
				restored, process.ID, changeover.ID)
		}

		// Check if changeover can be completed (all orders terminal, all nodes done)
		_ = e.tryCompleteProcessChangeover(process.ID)
	}
}

// reconcileNodeTask checks linked orders for a changeover node task and
// advances the node task state if orders completed while Edge was down.
// Returns true if any state advancement was made.
func (e *Engine) reconcileNodeTask(task *store.ChangeoverNodeTask) bool {
	advanced := false

	// Check staging/delivery order (NextMaterialOrderID)
	if task.NextMaterialOrderID != nil {
		if order, err := e.db.GetOrder(*task.NextMaterialOrderID); err == nil {
			if orders.IsTerminal(order.Status) {
				switch task.State {
				case "staging_requested":
					_ = e.db.UpdateChangeoverNodeTaskState(task.ID, "staged")
					advanced = true
				case "release_requested":
					_ = e.db.UpdateChangeoverNodeTaskState(task.ID, "released")
					advanced = true
				}
			}
		}
	}

	// Check evacuation/clear order (OldMaterialReleaseOrderID)
	if task.OldMaterialReleaseOrderID != nil {
		if order, err := e.db.GetOrder(*task.OldMaterialReleaseOrderID); err == nil {
			if orders.IsTerminal(order.Status) && task.State == "empty_requested" {
				_ = e.db.UpdateChangeoverNodeTaskState(task.ID, "line_cleared")
				advanced = true
			}
		}
	}

	return advanced
}
