package engine

import (
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

	// Activate this node, deactivate the partner
	if err := e.db.SetActivePull(nodeID, true); err != nil {
		log.Printf("ab_cycling: set active pull for node %d: %v", nodeID, err)
		}
	if err := e.db.SetActivePull(pairedNode.ID, false); err != nil {
		log.Printf("ab_cycling: set active pull for node %d: %v", pairedNode.ID, err)
		}

	log.Printf("A/B flip: node %s now active, node %s inactive", node.Name, pairedNode.Name)

	// Trigger auto-reorder on the depleted partner if needed
	if process.ActiveStyleID != nil {
		pairedClaim, _ := e.db.GetStyleNodeClaimByNode(*process.ActiveStyleID, pairedNode.CoreNodeName)
		pairedRuntime, _ := e.db.GetProcessNodeRuntime(pairedNode.ID)
		if pairedClaim != nil && pairedRuntime != nil &&
			pairedClaim.AutoReorder && pairedRuntime.RemainingUOP <= pairedClaim.ReorderPoint {
			if ok, _ := e.CanAcceptOrders(pairedNode.ID); ok {
				if _, err := e.RequestNodeMaterial(pairedNode.ID, 1); err != nil {
					log.Printf("A/B flip auto-reorder for depleted node %s: %v", pairedNode.Name, err)
				}
			}
		}
	}

	return nil
}
