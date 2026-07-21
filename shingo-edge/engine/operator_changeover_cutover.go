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
// drifts independently of the other. Returns (false, blockers, nil) when
// blocked, with one structured blocker per reason — the HMI handler
// surfaces these so operators see "task at node ALN_002 in
// staging_requested; order 703 in in_transit" rather than a generic 500.
//
// Blockers are structured (rather than the flat []string this used to
// return) so the same computation feeds BOTH the click-time toast and the
// live "waiting on:" panel. The gate already knew every fact the operator
// needs; it was formatting them into a string and throwing the structure
// away. domain.BlockersToReasons projects back to the old flat list, which
// is what keeps the 400 toast byte-identical.
//
// Every blocker is Hard today: both conjuncts are hard, and there is no
// override. See domain.Blocker for why the flag exists anyway.
func (e *Engine) canCompleteChangeover(changeoverID int64) (bool, []domain.Blocker, error) {
	tasks, err := e.db.ListChangeoverNodeTasks(changeoverID)
	if err != nil {
		return false, nil, err
	}
	var blockers []domain.Blocker
	for _, task := range tasks {
		if !domain.IsNodeTaskStateTerminal(task.State, task.Situation) {
			blockers = append(blockers, domain.Blocker{
				Reason:   fmt.Sprintf("task at node %s in %s", task.NodeName, task.State),
				NodeName: task.NodeName,
				Hard:     true,
			})
		}
	}
	for _, orderID := range linkedOrderIDs(tasks) {
		order, err := e.db.GetOrder(orderID)
		if err != nil {
			// A missing order row is deliberately NOT a blocker: the order was
			// GC'd or never persisted, and refusing cutover over a row nobody
			// can act on would wedge the changeover permanently.
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return false, nil, err
		}
		if !protocol.IsTerminal(order.Status) {
			blockers = append(blockers, domain.Blocker{
				Reason:  fmt.Sprintf("order %d in %s", order.ID, order.Status),
				OrderID: order.ID,
				Hard:    true,
			})
		}
	}
	if len(blockers) > 0 {
		return false, blockers, nil
	}
	return true, nil, nil
}

// ChangeoverGateStatus is the read-only projection of the cutover gate for
// the active changeover on a process: can it complete, and if not, what is it
// waiting on. Pure read — it resolves the changeover, evaluates the same gate
// completeCutover runs, and mutates nothing, so the HMI can poll it.
//
// Returns (true, nil, nil) when no changeover is active: there is nothing to
// gate, which the panel renders as "no changeover" rather than as an error.
func (e *Engine) ChangeoverGateStatus(processID int64) (bool, []domain.Blocker, error) {
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil, nil
		}
		return false, nil, err
	}
	return e.canCompleteChangeover(changeover.ID)
}

// linkedOrderIDs returns the deduped order IDs referenced by a changeover's
// node tasks — both the next-material and old-material-release legs — in
// first-seen order. canCompleteChangeover and the cutover auto-confirm
// pre-pass share it so they reason over exactly the same order set.
func linkedOrderIDs(tasks []processes.NodeTask) []int64 {
	seen := map[int64]struct{}{}
	var ids []int64
	for _, task := range tasks {
		for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
			if orderID == nil {
				continue
			}
			if _, dup := seen[*orderID]; dup {
				continue
			}
			seen[*orderID] = struct{}{}
			ids = append(ids, *orderID)
		}
	}
	return ids
}

// autoConfirmDeliveredLinkedOrders confirms any changeover-linked order the
// fleet has already delivered but the operator hasn't acknowledged. A
// delivered-but-unconfirmed order is non-terminal, so it would otherwise
// block canCompleteChangeover even though the material is physically on the
// line. ConfirmDelivery emits the completion synchronously, so the gate that
// runs immediately after sees the order as confirmed.
//
// in_transit/staged/faulted orders are left untouched — they still block the
// gate, as intended. On a ConfirmDelivery error the order is left delivered
// so the gate reports it rather than masking a real failure.
func (e *Engine) autoConfirmDeliveredLinkedOrders(changeoverID int64) error {
	tasks, err := e.db.ListChangeoverNodeTasks(changeoverID)
	if err != nil {
		return err
	}
	for _, orderID := range linkedOrderIDs(tasks) {
		order, err := e.db.GetOrder(orderID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		if order.Status != protocol.StatusDelivered {
			continue
		}
		if err := e.orderMgr.ConfirmDelivery(order.ID, order.Quantity); err != nil {
			log.Printf("cutover: auto-confirm delivered order %d failed, leaving for gate: %v", order.ID, err)
		}
	}
	return nil
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
	// Auto-confirm any linked order the fleet already delivered, so the gate
	// below isn't blocked on an operator clerical step for material that is
	// physically on the line. in_transit/staged/faulted still block.
	if err := e.autoConfirmDeliveredLinkedOrders(changeover.ID); err != nil {
		return err
	}
	// Gate must run before any of the five mutations below. The function
	// flips active_style_id (line below) before writing the completed row;
	// inserting the gate after the flip would leave the system on the
	// to-style with an still-in-progress changeover row if the gate
	// blocked. findActiveClaim resolves from process.ActiveStyleID, so
	// that order is unrecoverable without operator intervention.
	// BlockersToReasons keeps this message byte-identical to what it produced
	// before blockers became structured — the 400 toast is a contract the
	// floor reads, and the panel is additive to it, not a replacement.
	if ok, blockers, err := e.canCompleteChangeover(changeover.ID); err != nil || !ok {
		if err != nil {
			return err
		}
		return fmt.Errorf("cannot cutover: %s", strings.Join(domain.BlockersToReasons(blockers), "; "))
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
	return e.db.UpdateProcessChangeoverStateWithTrigger(changeoverID, domain.ChangeoverCompleted, triggeredBy)
}

func (e *Engine) tryCompleteProcessChangeover(processID int64) error {
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return err
	}
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // no active changeover — nothing to complete
	}
	if err != nil {
		// Real DB error (not the everyday "no active changeover"): keep the same
		// no-op control flow, but surface it so a changeover left open by a
		// transient read failure is diagnosable instead of silent.
		log.Printf("changeover: get active changeover for process %d: %v", processID, err)
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
		if err := e.db.UpdateChangeoverStationTaskState(task.ID, domain.StationTaskSwitched); err != nil {
			log.Printf("changeover: update station task state: %v", err)
		}
	}
	return e.finalizeChangeoverRow(processID, changeover.ID, "auto-task-terminal")
}
