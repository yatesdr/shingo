package engine

import (
	"database/sql"
	"fmt"
	"log"
	"shingo/protocol"
	"shingoedge/orders"

	"shingoedge/store/processes"
)

// flipTargetReady returns "" when the line may safely be put onto this position,
// or an operator-readable reason why not.
//
// ── THE INVARIANT CARRIES THE KNOWLEDGE ───────────────────────────────────
//
// The Edge holds no bin table: it cannot read the carrier type or the payload of
// whatever is standing on a position. It does not need to. Each arm below is a
// fact it already owns, and each leans on the same steady-state invariant the
// reuse-skip does — a produce press's parked side holds an empty of the running
// style's carrier, because steady state put it there.
//
//	SKIPPED        the reuse-shortcut turned this side's diff Unchanged, which
//	               it does only when the catalog says both styles ride the SAME
//	               carrier. The empty already standing there IS the one the new
//	               style wants. Nothing was ordered because nothing was needed.
//
//	DELIVERED      this side's own changeover order reached `delivered` or
//	               `confirmed`. The Edge WATCHED its robot deliver — a new carrier
//	               on produce, new material on consume. On consume it also checks
//	               the runtime is pointing at the incoming style's claim with
//	               material on it, because "an order finished" and "the right stuff
//	               is there" are two statements and consume is the role where they
//	               can differ.
//
//	ENDED WITHOUT  `failed`, `cancelled` or `skipped`. Terminal, and the opposite
//	  A DELIVERY   of ready: the order is over and no carrier came, so the position
//	               still holds the OLD style's. It gets its own sentence because
//	               "has not delivered yet" tells the operator to wait and there is
//	               nothing to wait for — the fix is another order, not patience.
//
//	STEADY STATE   no changeover is running, so there is nothing to be ready FOR
//	               beyond a bin being present — the invariant covers the rest.
//
// Every failure to READ answers ready(""). This guard exists to catch the
// operator's honest mistake, not to wall him out of his own press when a query
// hiccups; and it is confirm-overridable anyway.
func (e *Engine) flipTargetReady(node *processes.Node) string {
	rt, err := e.db.GetProcessNodeRuntime(node.ID)
	if err != nil || rt == nil {
		return ""
	}
	changeover, err := e.db.GetActiveProcessChangeover(node.ProcessID)
	if err != nil || changeover == nil {
		// STEADY STATE.
		if rt.ActiveBinID == nil {
			return fmt.Sprintf("%s has no bin on it", node.CoreNodeName)
		}
		return ""
	}
	task, err := e.db.GetChangeoverNodeTaskByNode(changeover.ID, node.ID)
	if err != nil || task == nil {
		return ""
	}
	if task.Situation == string(SituationUnchanged) {
		return "" // SKIPPED — same carrier, the resident empty is already correct
	}
	if task.NextMaterialOrderID == nil {
		return ""
	}
	order, oErr := e.db.GetOrder(*task.NextMaterialOrderID)
	if oErr != nil || order == nil {
		return ""
	}
	// ── TERMINAL IS NOT DELIVERED ─────────────────────────────────────────
	//
	// This asked !IsTerminal, which reads a CANCELLED or FAILED changeover order
	// as "the Edge watched its robot deliver". Those statuses are terminal and
	// mean the opposite: the order ended with no carrier delivered, so the
	// position is still holding the outgoing style's, and the flip was permitted
	// onto it with the warning off. Consume's extra conjuncts below catch that by
	// accident; produce has no second question, so on a produce press it was
	// silent — the exact failure this guard exists to prevent.
	//
	// So the arm tests the two statuses that actually mean delivered, and the
	// unready shapes each get the sentence that names what the operator has to do.
	switch order.Status {
	case orders.StatusDelivered, orders.StatusConfirmed:
		// The delivery happened. Fall through to consume's own questions.
	case orders.StatusFailed, orders.StatusCancelled, orders.StatusSkipped:
		return fmt.Sprintf("%s's changeover order %d ended %s — no new carrier was delivered, so this "+
			"position still holds the outgoing style's; order another before flipping",
			node.CoreNodeName, order.ID, order.Status)
	default:
		return fmt.Sprintf("%s's changeover order %d has not delivered (%s) — release it first",
			node.CoreNodeName, order.ID, order.Status)
	}
	// ── CONSUME'S EXTRA CONJUNCT, AND IT IS TWO QUESTIONS ─────────────────
	//
	// "The order finished" is not enough on a consume position: an empty carrier
	// there feeds the line nothing. So it must also hold MATERIAL, and that
	// material must be the INCOMING style's — a full bin of the outgoing part is
	// exactly as useless to a press about to run the new one.
	//
	// Both are edge-local. remaining_uop_cached answers "is there material".
	//
	// active_claim_id ANSWERS "WHOSE" ONLY BECAUSE THE CHANGEOVER DELIVERY MOVES
	// IT, and this comment used to claim more than that: "it names the claim the
	// resident bin was stocked for". It does not, and swap_evac_dest.go carries
	// the retraction of that exact sentence. It names the claim the node's
	// process was on when the field was last written, and most writers derive
	// that from the active style. What makes the comparison below meaningful is
	// applyChangeoverRelease and applyStagedDelivery advancing it when the
	// changeover's OWN material lands — before that fix it read as "the outgoing
	// style" for a median 18 minutes and refused flips that were ready.
	//
	// Unreadable to-claim answers ready(""): this guard catches the operator's
	// honest mistake, not a query hiccup, and it is confirm-overridable anyway.
	claim := e.claimAtNode(node)
	if claim != nil && claim.Role == protocol.ClaimRoleConsume {
		if rt.RemainingUOPCached <= 0 {
			return fmt.Sprintf("%s holds no material to feed the line", node.CoreNodeName)
		}
		toClaim, tErr := e.db.GetStyleNodeClaimByNode(changeover.ToStyleID, node.CoreNodeName)
		if tErr == nil && toClaim != nil && rt.ActiveClaimID != nil && *rt.ActiveClaimID != toClaim.ID {
			return fmt.Sprintf("%s still holds the outgoing style's material — release it first",
				node.CoreNodeName)
		}
	}
	return ""
}

// pairedNodeOf resolves the other half of an A/B pair from a node's active claim.
//
// The release trunk's guard (linePullsFrom) and its flip (releaseFlipPartner)
// both go through it, so the question and the write cannot disagree about
// which row the partner is.
func (e *Engine) pairedNodeOf(node *processes.Node) (*processes.Node, error) {
	claim := e.claimAtNode(node)
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.PairedCoreNode == "" {
		return nil, fmt.Errorf("node %s is not part of an A/B pair", node.Name)
	}
	nodes, err := e.db.ListProcessNodesByProcess(node.ProcessID)
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		if nodes[i].CoreNodeName == claim.PairedCoreNode {
			return &nodes[i], nil
		}
	}
	return nil, fmt.Errorf("paired node %s not found", claim.PairedCoreNode)
}

// writePullSide puts the pull bit on one side of a pair and takes it off the
// other, in one transaction. The canonical writer of active_pull. The one
// other writer is tooling evacuate's clear (changeover_applier.go), which
// sets both sides dark deliberately. writePullSide's one production caller is
// the release trunk's releaseFlipPartner — releasing a sequential position is
// the statement that the line has moved, taken as one atomic fact. (The
// operator flip door, Engine.FlipABNode, was deleted with its last caller.)
//
// Item 5 atomic wrap: a tick firing between the two writes — with both sides
// momentarily reading inactive, or both active — attributes to the wrong bucket.
// One SQLite transaction makes the pair atomic from the tick path's point of
// view, and factoring it here is what stops the operator's declaration and the
// flip drifting into two different ideas of what "the pair" means.
func (e *Engine) writePullSide(activeID, partnerID int64) error {
	if err := e.db.Transaction(func(tx *sql.Tx) error {
		if err := processes.SetActivePull(tx, activeID, true); err != nil {
			return fmt.Errorf("set active pull node=%d: %w", activeID, err)
		}
		if err := processes.SetActivePull(tx, partnerID, false); err != nil {
			return fmt.Errorf("set active pull paired-node=%d: %w", partnerID, err)
		}
		return nil
	}); err != nil {
		log.Printf("ab_cycling: atomic pull write node=%d paired=%d: %v", activeID, partnerID, err)
		return err
	}
	return nil
}
