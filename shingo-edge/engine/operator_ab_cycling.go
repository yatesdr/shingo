package engine

import (
	"database/sql"
	"fmt"
	"log"

	"shingoedge/store/processes"
)

// FlipABNode switches the active pull point to the specified node and deactivates
// its paired partner. Used for A/B cycling — operator (or PLC bit) decides when
// to start pulling from the other side. Triggers auto-reorder on the depleted node
// if the depleted node's UOP is at or below its reorder point.
func (e *Engine) FlipABNode(nodeID int64) error {
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil {
		return fmt.Errorf("node not found: %w", err)
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
				if _, err := e.RequestNodeMaterial(pairedNode.ID, 1); err != nil {
					log.Printf("A/B flip auto-reorder for depleted node %s: %v", pairedNode.Name, err)
				}
			}
		}
	}

	return nil
}
