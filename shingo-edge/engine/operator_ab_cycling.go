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
// to start pulling from the other side. Triggers auto-reorder on the depleted node
// if the depleted node's UOP is at or below its reorder point.
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

	claim := findActiveClaim(e.db, node)
	if claim == nil {
		return fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.PairedCoreNode == "" {
		return fmt.Errorf("node %s is not part of an A/B pair", node.Name)
	}

	// Find the paired node
	process, err := e.db.GetProcess(node.ProcessID)
	if err != nil {
		return err
	}
	nodes, err := e.db.ListProcessNodesByProcess(node.ProcessID)
	if err != nil {
		return err
	}
	var pairedNode *processes.Node
	for i := range nodes {
		if nodes[i].CoreNodeName == claim.PairedCoreNode {
			pairedNode = &nodes[i]
			break
		}
	}
	if pairedNode == nil {
		return fmt.Errorf("paired node %s not found", claim.PairedCoreNode)
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

	// Item 5 atomic wrap: the two SetActivePull writes flip a paired
	// node's active state. A tick firing between the two writes (with
	// both sides momentarily seeing themselves inactive, or both
	// active) would attribute to the wrong bucket. Wrapping the pair
	// in a single SQLite transaction makes the flip atomic from the
	// tick path's POV.
	if err := e.db.Transaction(func(tx *sql.Tx) error {
		if err := processes.SetActivePull(tx, nodeID, true); err != nil {
			return fmt.Errorf("set active pull node=%d: %w", nodeID, err)
		}
		if err := processes.SetActivePull(tx, pairedNode.ID, false); err != nil {
			return fmt.Errorf("set active pull paired-node=%d: %w", pairedNode.ID, err)
		}
		return nil
	}); err != nil {
		log.Printf("ab_cycling: atomic flip node=%d paired=%d: %v", nodeID, pairedNode.ID, err)
		return err
	}

	log.Printf("A/B flip: node %s now active, node %s inactive", node.Name, pairedNode.Name)

	// Trigger auto-reorder on the depleted partner if needed
	if process.ActiveStyleID != nil {
		pairedClaim, _ := e.db.GetStyleNodeClaimByNode(*process.ActiveStyleID, pairedNode.CoreNodeName)
		pairedRuntime, _ := e.db.GetProcessNodeRuntime(pairedNode.ID)
		if pairedClaim != nil && pairedRuntime != nil &&
			pairedClaim.AutoReorder && pairedRuntime.RemainingUOPCached <= pairedClaim.ReorderPoint {
			if ok, _ := e.CanAcceptOrders(pairedNode.ID); ok {
				if _, err := e.requestNodeMaterialFor(pairedNode.ID, 1, protocol.EpisodeTriggerAutoreorder); err != nil {
					log.Printf("A/B flip auto-reorder for depleted node %s: %v", pairedNode.Name, err)
				}
			}
		}
	}

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
//	DELIVERED      this side's own changeover order reached a terminal status.
//	               The Edge WATCHED its robot deliver — a new carrier on produce,
//	               new material on consume. On consume it also checks the runtime
//	               is pointing at the incoming style's claim with material on it,
//	               because "an order finished" and "the right stuff is there" are
//	               two statements and consume is the role where they can differ.
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
	if !orders.IsTerminal(order.Status) {
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
