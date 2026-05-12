// operator_changeover_cutover.go — sequential per-node cutover and
// process-wide cutover/completion.
//
// SequentialChangeoverCutover handles the mid-order flip during a
// sequential SWAP. CompleteProcessProductionCutover (and its PLC twin)
// gate, flip active style, and finalize. tryCompleteProcessChangeover
// is the auto-completion path triggered by terminal-state events.

package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// SequentialChangeoverCutover is the per-node operator action that gates
// the active-side swap during a sequential SWAP changeover.
//
// Sequential SWAP ships a single complex order with a mid-sequence wait
// at the active position. The robot has finished swapping the inactive
// side and is parked at the active position. The operator clicks
// "cutover" to:
//
//  1. Flip ActivePull to the previously-inactive (now freshly-stocked)
//     side. The line starts pulling from the new bin immediately.
//  2. Release the wait inside the running complex order so the robot
//     proceeds to evac the now-inactive side and deliver the new bin.
//
// Order matters: flip BEFORE release. If the wait released first, the
// robot could begin pickup at a position the line is still pulling
// from. Atomic from the operator's POV (one HTTP call, server-side
// sequence is internal).
//
// nodeID is the changeover task's primary process node (CoreNodeName).
// The cutover handler re-reads ActivePull at the moment of the click to
// find which physical side is inactive — the planner-time resolution is
// not persisted, but ActivePull doesn't change between plan and cutover
// (the changeover itself doesn't flip; only this handler does).
func (e *Engine) SequentialChangeoverCutover(processID, nodeID int64, calledBy string) error {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return fmt.Errorf("sequential cutover: no active changeover for process %d: %w", processID, err)
	}
	task, err := e.db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		return fmt.Errorf("sequential cutover: get node task: %w", err)
	}
	if task.Situation != "swap" {
		return fmt.Errorf("sequential cutover: node task situation is %q, not swap", task.Situation)
	}
	if task.FromClaimID == nil {
		return fmt.Errorf("sequential cutover: node task has no from-claim id")
	}
	fromClaim, err := e.db.GetStyleNodeClaim(*task.FromClaimID)
	if err != nil || fromClaim == nil {
		return fmt.Errorf("sequential cutover: get from-claim: %w", err)
	}
	if fromClaim.SwapMode != protocol.SwapModeSequential {
		return fmt.Errorf("sequential cutover: from-claim swap_mode is %q, not sequential", fromClaim.SwapMode)
	}
	if fromClaim.PairedCoreNode == "" {
		return fmt.Errorf("sequential cutover: from-claim has no paired_core_node")
	}
	if task.NextMaterialOrderID == nil {
		return fmt.Errorf("sequential cutover: node task has no tracked complex order")
	}

	// Resolve inactive/active using the same logic the planner ran. The
	// inactive-node CoreNodeName names the physical node we're flipping
	// pull TO (it's been freshly stocked by the pre-cutover steps).
	processNode, err := e.db.GetProcessNode(task.ProcessNodeID)
	if err != nil {
		return fmt.Errorf("sequential cutover: get process node: %w", err)
	}
	nodes, err := e.db.ListProcessNodesByProcess(processNode.ProcessID)
	if err != nil {
		return fmt.Errorf("sequential cutover: list process nodes: %w", err)
	}
	activePull := e.activePullSnapshot(nodes)
	inactive, _ := resolveSequentialActivePull(fromClaim, activePull)
	if inactive == "" {
		return fmt.Errorf("sequential cutover: could not resolve inactive node from active-pull snapshot")
	}
	var inactivePhysical *processes.Node
	for i := range nodes {
		if nodes[i].CoreNodeName == inactive {
			inactivePhysical = &nodes[i]
			break
		}
	}
	if inactivePhysical == nil {
		return fmt.Errorf("sequential cutover: inactive node %q not found in process %d", inactive, processNode.ProcessID)
	}

	// 1. Flip first (so when the robot wakes, the line is already pulling
	// from the freshly-stocked side and the robot can safely evac the
	// now-stale active side).
	if err := e.FlipABNode(inactivePhysical.ID); err != nil {
		return fmt.Errorf("sequential cutover: flip active-pull to %s: %w", inactive, err)
	}

	// 2. Release the wait. The complex order's mid-sequence wait is at
	// the active position; releasing it lets the robot proceed.
	disp := ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: calledBy}
	if err := e.ReleaseOrderWithLineside(*task.NextMaterialOrderID, disp); err != nil {
		return fmt.Errorf("sequential cutover: release wait on order %d: %w", *task.NextMaterialOrderID, err)
	}
	log.Printf("sequential changeover: cutover at node %s (process=%d task=%d) — flipped pull to %s, released order %d",
		task.NodeName, processID, task.ID, inactive, *task.NextMaterialOrderID)
	return nil
}

// canCompleteChangeover reports whether a changeover row may transition to
// "completed". Both checks are required:
//
//  1. Every changeover_node_tasks row must be in a terminal state (per
//     domain.IsNodeTaskStateTerminal).
//  2. Every order referenced by a node task (NextMaterialOrderID,
//     OldMaterialReleaseOrderID) must be in a terminal status (per
//     protocol.IsTerminal).
//
// Pinning both checks keeps the gate honest if either state machine
// drifts independently of the other. Returns (false, reasons, nil) when
// blocked, with one human-readable line per blocker — the HMI handler
// surfaces these so operators see "task at node ALN_002 in
// staging_requested; order 703 in in_transit" rather than a generic 500.
func (e *Engine) canCompleteChangeover(changeoverID int64) (bool, []string, error) {
	tasks, err := e.db.ListChangeoverNodeTasks(changeoverID)
	if err != nil {
		return false, nil, err
	}
	var reasons []string
	for _, task := range tasks {
		if !domain.IsNodeTaskStateTerminal(task.State, task.Situation) {
			reasons = append(reasons, fmt.Sprintf("task at node %s in %s", task.NodeName, task.State))
		}
	}
	seen := map[int64]struct{}{}
	for _, task := range tasks {
		for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
			if orderID == nil {
				continue
			}
			if _, dup := seen[*orderID]; dup {
				continue
			}
			seen[*orderID] = struct{}{}
			order, err := e.db.GetOrder(*orderID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return false, nil, err
			}
			if !protocol.IsTerminal(order.Status) {
				reasons = append(reasons, fmt.Sprintf("order %d in %s", order.ID, order.Status))
			}
		}
	}
	if len(reasons) > 0 {
		return false, reasons, nil
	}
	return true, nil, nil
}

// CompleteProcessProductionCutover runs the operator-driven cutover:
// gate → flip active style → finalize. Trigger source is recorded as
// "operator-hmi" on the changeover row.
func (e *Engine) CompleteProcessProductionCutover(processID int64) error {
	return e.completeCutover(processID, "operator-hmi")
}

// CompleteProcessProductionCutoverFromPLC is the entry point used by
// the PLC-driven cutover monitor. Identical to the operator-driven
// path except the changeover row records "plc-auto" as the trigger
// source for audit/postmortem.
func (e *Engine) CompleteProcessProductionCutoverFromPLC(processID int64) error {
	return e.completeCutover(processID, "plc-auto")
}

func (e *Engine) completeCutover(processID int64, triggeredBy string) error {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return err
	}
	// Gate must run before any of the five mutations below. The function
	// flips active_style_id (line below) before writing the completed row;
	// inserting the gate after the flip would leave the system on the
	// to-style with an still-in-progress changeover row if the gate
	// blocked. findActiveClaim resolves from process.ActiveStyleID, so
	// that order is unrecoverable without operator intervention.
	if ok, reasons, err := e.canCompleteChangeover(changeover.ID); err != nil || !ok {
		if err != nil {
			return err
		}
		return fmt.Errorf("cannot cutover: %s", strings.Join(reasons, "; "))
	}
	toStyleID := changeover.ToStyleID
	if err := e.db.SetActiveStyle(processID, &toStyleID); err != nil {
		return err
	}
	return e.finalizeChangeoverRow(processID, changeover.ID, triggeredBy)
}

// finalizeChangeoverRow runs the post-gate, post-flip steps shared by
// CompleteProcessProductionCutover and tryCompleteProcessChangeover:
// clear target style, mark production active, sync the counter
// reporting-point's style_id, and write the completed row.
//
// Step order is load-bearing: restoreChangeoverState reads
// (active_style, changeover.state) jointly during crash recovery and the
// invariant it relies on is "active_style flipped ⇒ changeover writeable
// to completed." Reordering would break that recovery contract.
//
// SyncProcessCounter is included here so the auto-completion path
// (tryCompleteProcessChangeover) keeps the reporting point's style_id
// in sync — without this, a PLC- or event-driven cutover via the auto
// path would land with the reporting point still pointing at the
// from-style.
func (e *Engine) finalizeChangeoverRow(processID, changeoverID int64, triggeredBy string) error {
	if err := e.db.SetTargetStyle(processID, nil); err != nil {
		return err
	}
	if err := e.db.SetProcessProductionState(processID, "active_production"); err != nil {
		return err
	}
	if err := e.SyncProcessCounter(processID); err != nil {
		return err
	}
	return e.db.UpdateProcessChangeoverStateWithTrigger(changeoverID, "completed", triggeredBy)
}

func (e *Engine) tryCompleteProcessChangeover(processID int64) error {
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return err
	}
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return nil
	}
	if process.ActiveStyleID == nil || *process.ActiveStyleID != changeover.ToStyleID {
		return nil
	}
	// Gate before the station-task force-switch. Today's auto-completion
	// path checked node-task terminality only; the broader gate also
	// requires linked orders to be terminal so a late-arriving order
	// completion doesn't leave a node task stranded after the row is
	// closed.
	ok, _, err := e.canCompleteChangeover(changeover.ID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	tasks, err := e.db.ListChangeoverStationTasks(changeover.ID)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if err := e.db.UpdateChangeoverStationTaskState(task.ID, "switched"); err != nil {
			log.Printf("changeover: update station task state: %v", err)
		}
	}
	return e.finalizeChangeoverRow(processID, changeover.ID, "auto-task-terminal")
}
