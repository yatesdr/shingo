package engine

import (
	"database/sql"
	"fmt"
	"log"
	"shingo/protocol"
	"shingoedge/orders"

	"shingoedge/store/processes"
)

// FlipABNode switches the active pull point to the specified node and deactivates
// its paired partner. Used for A/B cycling — operator (or PLC bit) decides when
// to start pulling from the other side.
// FlipRequest says who is asking to flip and whether they have looked.
//
// A PLC bit cannot look at the aisle, so it can never carry Confirm — a flip it
// asks for onto an unready position is refused loudly rather than overridden
// (the changeover-53 precedent). An operator can, because he can.
type FlipRequest struct {
	Confirm  bool
	CalledBy string
	ByPLC    bool
}

// OperatorFlip is the ordinary unconfirmed operator request, and the zero value
// most callers want.
func OperatorFlip(calledBy string) FlipRequest { return FlipRequest{CalledBy: calledBy} }

func (e *Engine) FlipABNode(nodeID int64, req FlipRequest) error {
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil {
		return fmt.Errorf("node not found: %w", err)
	}
	// ── DO NOT PUT THE LINE ONTO A POSITION THAT CANNOT FEED IT ───────────
	//
	// The flip is what makes the OTHER side releasable, so it is where the
	// "has the operator got a bin of the new product" question belongs. Every
	// arm below is answered from state this Edge already owns — no Core call,
	// no per-node inventory read. See flipTargetReady.
	if why := e.flipTargetReady(node); why != "" {
		if req.ByPLC {
			log.Printf("A/B flip REFUSED (PLC) node=%s: %s — a PLC bit cannot see the aisle, so it "+
				"cannot override this; a person must look and flip from the board", node.CoreNodeName, why)
			return fmt.Errorf("flip to %s refused: %s", node.CoreNodeName, why)
		}
		if !req.Confirm {
			return fmt.Errorf("%s; confirm to flip anyway", why)
		}
		log.Printf("AUDIT flip-override: node=%s called_by=%q — %s; the operator flipped anyway",
			node.CoreNodeName, req.CalledBy, why)
	}

	pairedNode, err := e.pairedNodeOf(node)
	if err != nil {
		return err
	}
	// Attribution boundary: A/B cycling has no operator action at the
	// inactive→active transition — the active-pull state flip IS the
	// boundary. Without flushing here the inactive node's accumulator
	// would carry residual deltas past the flip and they'd ship under
	// the wrong active-bin attribution. Fires before the SetActivePull
	// writes so the outgoing-bin's deltas land before the new bin
	// starts driving ticks against the now-active node.
	//
	// MarkAttributionBoundary is synchronous — a returned error means
	// the flush failed and we must NOT proceed with the SetActivePull
	// swap (pending deltas would land under the wrong attribution).
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.MarkAttributionBoundary(nodeID); err != nil {
			return fmt.Errorf("attribution boundary flush failed: %w", err)
		}
	}

	if err := e.writePullSide(nodeID, pairedNode.ID); err != nil {
		return err
	}

	log.Printf("A/B flip: node %s now active, node %s inactive", node.Name, pairedNode.Name)

	// THE DEPLETED PARTNER IS THE LEVEL SWEEP'S DECISION, TAKEN EARLY HERE.
	//
	// A tail used to sit at this line firing its own auto-reorder, and it was
	// the one order-firing site that answered to nothing: no reorder_point > 0
	// opt-out, so a claim set to the documented opt-out fired from here anyway;
	// no check for a bin already inbound; no hysteresis; and it never called
	// evaluateCellLevel, so it ordered without recording that the cell had gone
	// below its level. All four now apply, because this is the same decision
	// the periodic sweep takes and not a second implementation of it.
	//
	// It is here at all only for immediacy. A flip is the one moment a parked
	// side becomes interesting, and waiting a period to notice is a real
	// regression on the operator-visible path. Drop this line and the sweep
	// still covers the cell on its next pass — it does not skip parked sides.
	e.sweepNodeLevelNow(pairedNode.ID)

	return nil
}

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
	// Both are edge-local. remaining_uop_cached answers "is there material", and
	// active_claim_id answers "whose" — it names the claim the resident bin was
	// stocked for, so comparing it against the to-style's claim for this node is
	// the whole question, with no bin table and no Core call.
	//
	// Unreadable to-claim answers ready(""): this guard catches the operator's
	// honest mistake, not a query hiccup, and it is confirm-overridable anyway.
	claim := findActiveClaim(e.db, node)
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
// Both writers of active_pull go through it, so neither can end up writing one
// bit while disagreeing about which row the partner is.
func (e *Engine) pairedNodeOf(node *processes.Node) (*processes.Node, error) {
	claim := findActiveClaim(e.db, node)
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
// other, in one transaction. THE ONLY PLACE EITHER WRITER TOUCHES active_pull.
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

// SetActivePullSide records which side of an A/B pair the line is DRAWING FROM,
// without moving anything.
//
// ── THE FLIP STAYS CANONICAL; THIS IS THE OTHER QUESTION ──────────────────
//
// FlipABNode remains the writer of active_pull in ordinary operation, and that
// is the point of it: it moves the line and writes the bit in the same click, so
// the two cannot disagree. The gap it does not cover is the state a tooling
// evacuate leaves — clearActivePullForEvacuate darkens BOTH sides, which is
// correct while the press is down, and nothing re-asserts the bit when it comes
// back up. Both sides then read 0, the release guard is silent on a running
// press, and the only existing click that lights the bit is a flip: a
// choreography step the operator may not want, because he may already be on the
// side he means to run.
//
// So this is the declaration, and it is deliberately NOT a flip:
//
//	NO READINESS GUARD. flipTargetReady asks "may the line be MOVED onto this
//	position". Nothing is being moved. The operator is telling the system what
//	is already true on the floor, and it is his eyes against a bit that is
//	currently blank.
//
//	NO AUTO-REORDER. Nothing was depleted; no side just came off the line.
//
//	THE ATTRIBUTION BOUNDARY STILL FIRES, for the same reason the flip fires it:
//	the bit decides which position a UOP tick lands against, so residual deltas
//	in the incoming side's accumulator must be flushed BEFORE it starts driving
//	ticks under a new attribution. A declaration that changed the answer without
//	flushing would ship the old bin's counts against the new one.
//
// AUDITED, AND CLOSED TO THE PLC. The whole content of this call is "a person
// looked at the aisle and this is what is true" — the same statement
// ConfirmActivePull and FlipRequest.Confirm carry, and it is logged the same
// way. A PLC bit cannot look, so it can never make it (the changeover-53
// precedent, and FlipABNode's own ByPLC arm).
func (e *Engine) SetActivePullSide(nodeID int64, req FlipRequest) error {
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil || node == nil {
		return fmt.Errorf("node not found: %w", err)
	}
	if req.ByPLC {
		log.Printf("active-pull declaration REFUSED (PLC) node=%s: a PLC bit cannot see which side the "+
			"line is drawing from; a person must look and set it from the board", node.CoreNodeName)
		return fmt.Errorf("setting the active pull side on %s is an operator action: a PLC cannot see "+
			"the aisle", node.CoreNodeName)
	}
	pairedNode, err := e.pairedNodeOf(node)
	if err != nil {
		return err
	}

	// Same flush, same ordering, same refusal as the flip — see writePullSide's
	// caller above and MarkAttributionBoundary's own doc.
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.MarkAttributionBoundary(nodeID); err != nil {
			return fmt.Errorf("attribution boundary flush failed: %w", err)
		}
	}
	if err := e.writePullSide(nodeID, pairedNode.ID); err != nil {
		return err
	}
	log.Printf("AUDIT active-pull set: node=%s partner=%s called_by=%q — the operator declared that the "+
		"line is drawing from %s; the release guard now protects it and %s is releasable",
		node.CoreNodeName, pairedNode.CoreNodeName, req.CalledBy,
		node.CoreNodeName, pairedNode.CoreNodeName)
	return nil
}
