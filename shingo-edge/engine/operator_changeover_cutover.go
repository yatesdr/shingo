// operator_changeover_cutover.go — process-wide cutover/completion.
//
// CompleteProcessProductionCutover (and its PLC twin) gate, flip active style,
// and finalize. tryCompleteProcessChangeover is the auto-completion path
// triggered by terminal-state events.
//
// SequentialChangeoverCutover USED TO LIVE HERE and is gone (owner ruling
// 2026-08-28). It bundled "flip the pull, then release the wait" into one
// changeover-only button, and then needed a precondition of its own to stop
// itself cutting over onto an unstocked position. Both halves already exist as
// ordinary, mode-agnostic operator controls — FlipABNode and the per-node
// release — so the cutover is those two clicks, each carrying its own physical
// guard. A changeover needs no ceremony the steady state does not.

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
