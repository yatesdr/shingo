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
// Sequential SWAP is now TWO orders, one per position (owner ruling
// 2026-08-28). The parked side's order ran immediately and has put a fresh bin
// on that position; the active side's order is parked at its OWN node behind an
// opening stationWait, holding until this click. The operator clicks "cutover"
// to:
//
//  1. Flip ActivePull to the previously-inactive (now freshly-stocked)
//     side. The line starts pulling from the new bin immediately.
//  2. Release the opening wait on the ACTIVE node's own order so its robot
//     proceeds to evac the now-inactive side and deliver the new bin.
//
// Order matters: flip BEFORE release. If the wait released first, the
// robot could begin pickup at a position the line is still pulling
// from. Atomic from the operator's POV (one HTTP call, server-side
// sequence is internal).
//
// ── AND THE PRECONDITION THE SPLIT MADE NECESSARY ─────────────────────────
//
// The whole-press order enforced one thing by being a single order: its cutover
// wait sat AFTER the parked side's four steps, so it could not be reached until
// the parked position had its fresh bin. Cutting over flips the line onto that
// position — if it is still empty, the line is switched onto nothing and a
// press that was running stops.
//
// Two orders sequence themselves against nothing, so that guarantee is stated
// here instead: refuse until the parked node's own order has put its bin down.
// One extra task read, against rows this handler already walks.
//
// WAIT, NOT FAIL. The refusal names the parked node and the order that will
// release it, because that is the difference between a button that is waiting
// and a button that is broken. The operator clicks again when the parked side
// lands and the same click goes through.
//
// nodeID is the changeover task's primary process node (CoreNodeName), which is
// the ACTIVE position — the one whose order is holding. The cutover handler
// re-reads ActivePull at the moment of the click to find which physical side is
// inactive — the planner-time resolution is not persisted, but ActivePull
// doesn't change between plan and cutover (the changeover itself doesn't flip;
// only this handler does).
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

	// 0. THE PARKED SIDE MUST HAVE ITS BIN. Before anything is flipped or
	// released — a gate that half-runs leaves the operator worse off than one
	// that refuses, so this precedes both mutations.
	if err := e.parkedSideRefilled(changeover.ID, inactivePhysical); err != nil {
		return err
	}

	// 1. Flip first (so when the robot wakes, the line is already pulling
	// from the freshly-stocked side and the robot can safely evac the
	// now-stale active side).
	if err := e.FlipABNode(inactivePhysical.ID); err != nil {
		return fmt.Errorf("sequential cutover: flip active-pull to %s: %w", inactive, err)
	}

	// 2. Release the wait. Per-node, this is the ACTIVE position's OWN order and
	// the wait is its opening step — the robot has been parked at this node
	// since dispatch. (It used to be a wait in the middle of a shared
	// whole-press order; same task field, same release call, one order shorter.)
	disp := ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: calledBy}
	if err := e.ReleaseOrderWithLineside(*task.NextMaterialOrderID, disp); err != nil {
		return fmt.Errorf("sequential cutover: release wait on order %d: %w", *task.NextMaterialOrderID, err)
	}
	log.Printf("sequential changeover: cutover at node %s (process=%d task=%d) — flipped pull to %s, released order %d",
		task.NodeName, processID, task.ID, inactive, *task.NextMaterialOrderID)
	return nil
}

// parkedSideRefilled reports whether the parked position's changeover order has
// finished putting its fresh bin down, as a refusal the operator can act on.
//
// ── WHY EACH "NOTHING TO WAIT FOR" ARM ALLOWS THE CLICK ───────────────────
//
// The gate exists to stop the line being flipped onto an EMPTY position, so it
// must only fire when there is a real, unfinished refill to wait for. Every
// other case would be a cutover button that can never be pressed:
//
//	NO TASK at the parked node — that position is not part of this changeover
//	(its style claim did not change), so nothing is being restocked there and
//	nothing is coming.
//
//	NO ORDER on the task — same conclusion from the other end: the plan
//	produced no material leg for that position.
//
//	THE ORDER ROW IS GONE — canCompleteChangeover rules this way on the same
//	question for the same reason: refusing forever over a row nobody can act on
//	wedges the changeover.
//
// DELIVERED COUNTS. The bin is physically standing on the position at
// `delivered`; what is missing is the confirm, which is clerical and which
// completeCutover's own pre-pass performs automatically for exactly this
// reason. Waiting for it here would block a cutover on paperwork for material
// the operator can see on the press.
func (e *Engine) parkedSideRefilled(changeoverID int64, parked *processes.Node) error {
	task, err := e.db.GetChangeoverNodeTaskByNode(changeoverID, parked.ID)
	if err != nil || task == nil {
		return nil // not part of this changeover — see above
	}
	if task.NextMaterialOrderID == nil {
		return nil
	}
	order, err := e.db.GetOrder(*task.NextMaterialOrderID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("sequential cutover: read parked node %s's order %d: %w",
			parked.CoreNodeName, *task.NextMaterialOrderID, err)
	}
	if protocol.IsTerminal(order.Status) || order.Status == protocol.StatusDelivered {
		return nil
	}
	return fmt.Errorf("cannot cut over yet: the parked position %s is still being restocked by order "+
		"%d (%s). Cutting over switches the line onto %s, so its new bin has to be standing there "+
		"first — click cutover again when that order delivers",
		parked.CoreNodeName, order.ID, order.Status, parked.CoreNodeName)
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
	// Conjunct 2′ — an order gates cutover only when it PLACES A BIN at a
	// participant node (Amendment 1: the gate is conjuncts 1 + 2′, Edge-local).
	//
	// The old conjunct required EVERY linked order terminal, which is what
	// blocked the HOP cutover on an outbound evac still driving its old bin to
	// the market — a leg whose completion changes nothing at any node the
	// changeover touches. The classifier is legPlacesBinAt over the order's own
	// steps: "does this leg leave a bin at node n", the same question both
	// sides of the wire ask. Evac legs never gate here; their physical
	// slot-clear moment already flows into conjunct 1 (evac pickup advances a
	// drop task to line_cleared, which is terminal for drop).
	//
	// THIS SAME EXPRESSION IS THE HARD CONJUNCT. There is no separate hard
	// code path to drift; Blocker.Hard stays display-only.
	//
	// Fail-closed rule: an order whose steps cannot be read — decode failure,
	// or NO steps at all (simple move orders) — GATES, exactly as every order
	// did before 2′. The classifier only UN-gates legs it can PROVE place no
	// bin at a participant. delivery_node is deliberately never consulted: the
	// Edge leaves it blank by design on evac legs, so a delivery_node
	// predicate fails open — the wrong direction for dual-dispatch prevention.
	participants, err := e.db.ListChangeoverParticipants(changeoverID)
	if err != nil {
		return false, nil, err
	}
	participantNames := make([]string, 0, len(participants))
	for _, p := range participants {
		participantNames = append(participantNames, p.CoreNodeName)
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
		if protocol.IsTerminal(order.Status) {
			continue
		}
		if !e.orderGatesCutover(order.ID, participantNames) {
			continue
		}
		blockers = append(blockers, domain.Blocker{
			Reason:  fmt.Sprintf("order %d in %s", order.ID, order.Status),
			OrderID: order.ID,
			Hard:    true,
		})
	}
	if len(blockers) > 0 {
		return false, blockers, nil
	}
	return true, nil, nil
}

// orderGatesCutover answers one question per order: does this order place a bin
// at any node the changeover is reconfiguring? An order whose shape cannot be
// read returns true, i.e. it blocks — the safe answer for exactly the orders
// nothing can be proven about.
//
// WHY BLOCKING-WHEN-THERE-IS-NO-ITINERARY IS SAFE HERE, AND WHAT WOULD BREAK IT.
//
// Single-leg orders carry no itinerary, so this returns true for every one of
// them without ever checking where they actually go. That reads like
// over-blocking, and at the changeover START gate it was: that gate walks every
// live order in the plant, so one single-leg order delivering somewhere
// unrelated blocked every changeover. It was fixed there by checking the
// itinerary for multi-leg orders and the delivery node for single-leg ones
// (orderPlacesBinAtAny, swap_leg_role.go).
//
// Here the same shortcut changes nothing, because this gate only ever sees the
// changeover's OWN linked orders, and two things are true of that set:
//
//   - every SINGLE-LEG order a changeover creates delivers TO a node being
//     reconfigured. Both RetrieveOrderSpec sites set DeliveryNode to
//     toClaim.CoreNodeName (changeover_planner.go), because a single-leg supply
//     leg exists precisely to stock the node being reconfigured.
//   - every EVACUATION leg is MULTI-LEG (complexSpecWithPayload, same file).
//     Those are the legs that legitimately must not block: an outbound bin
//     heading to the market, whose completion changes nothing at any node being
//     reconfigured. That is the Hopkinsville case this rule was written for, and
//     they carry an itinerary, so they get classified properly.
//
// So blocking-on-no-itinerary and a real destination check give the SAME answer
// for every order that can reach here. Adopting the shared helper would change
// no behaviour.
//
// IF SOMEONE ADDS A SINGLE-LEG EVACUATION SPEC, that stops being true: the leg
// would carry no itinerary, block, and hold up a cutover it has no bearing on,
// reviving the exact problem this rule removed. At that point switch this to
// orderPlacesBinAtAny.
func (e *Engine) orderGatesCutover(orderID int64, participantNames []string) bool {
	stepsJSON, err := e.db.GetOrderStepsJSON(orderID)
	if err != nil || stepsJSON == "" {
		return true // cannot read the leg's shape — block, per the note above
	}
	steps, err := decodeSteps(stepsJSON)
	if err != nil {
		return true // an unreadable itinerary blocks
	}
	for _, name := range participantNames {
		if legPlacesBinAt(steps, name) {
			return true
		}
	}
	return false
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
	if err := e.db.UpdateProcessChangeoverStateWithTrigger(changeoverID, domain.ChangeoverCompleted, triggeredBy); err != nil {
		return err
	}
	e.closeChangeoverEpisode(changeoverID, protocol.CloseReasonChangeoverComplete, protocol.ClosedByNotification)
	// Open the post-cutover part-id verification watch: within a short window, if
	// the press's live CATID still disagrees with the new active style, this
	// changeover is flagged for operator confirmation on the station.
	e.openPostCutoverVerify(processID, changeoverID)
	return nil
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
	// PARITY with the operator path: auto-confirm any linked order the fleet
	// already delivered before evaluating the gate. completeCutover has run
	// this pre-pass since it existed; this path never did, so an auto-cutover
	// could hang forever on a clerical confirm no operator was looking at —
	// two definitions of "ready" for the same gate. Errors log and defer to
	// the next trigger rather than failing the auto path.
	if err := e.autoConfirmDeliveredLinkedOrders(changeover.ID); err != nil {
		log.Printf("changeover: auto-path pre-pass for changeover %d: %v", changeover.ID, err)
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
