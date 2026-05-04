package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
)

// AdminAdjustLinesideUOP is the admin override for a slot's lineside UOP, exposed
// via the team-leader / engineer-only "Lineside Buckets" page on edge. Two ops:
//
//   - Edit count (clearBin=false): caps targetUOP to [0, claim.UOPCapacity], emits
//     a BinUOPDelta with reason ReasonOperatorCorrection so Core's bins.uop_remaining
//     mirrors, and writes the new count to the runtime row.
//
//   - Clear slot (clearBin=true): nulls active_bin_id and zeroes the runtime cache.
//     Does NOT emit a bin delta — the engineer is admitting state drift, not asserting
//     the bin's contents. Bin row on Core stays as-is for inventory purposes.
//
// Normal release → drop cycles still drive routine HMI updates; this is for
// unstuck scenarios where edge state diverged and someone needs to realign
// without restarting flow.
func (e *Engine) AdminAdjustLinesideUOP(nodeID int64, targetUOP int, clearBin bool) error {
	node, runtime, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return fmt.Errorf("load node %d: %w", nodeID, err)
	}

	if clearBin {
		var nilBin *int64
		if err := e.db.SetProcessNodeRuntimeWithBin(nodeID, runtime.ActiveClaimID, nilBin, 0); err != nil {
			return fmt.Errorf("clear runtime for node %s: %w", node.Name, err)
		}
		log.Printf("admin_lineside: cleared slot on node %s (was bin=%v, uop=%d)",
			node.Name, runtime.ActiveBinID, runtime.RemainingUOPCached)
		return nil
	}

	if claim == nil {
		return fmt.Errorf("node %s has no active claim — can't edit count", node.Name)
	}
	if runtime.ActiveBinID == nil {
		return fmt.Errorf("node %s has no bin loaded — nothing to edit", node.Name)
	}
	if targetUOP < 0 {
		return fmt.Errorf("uop count cannot be negative")
	}
	if targetUOP > claim.UOPCapacity {
		return fmt.Errorf("uop count %d exceeds capacity %d", targetUOP, claim.UOPCapacity)
	}

	delta := targetUOP - runtime.RemainingUOPCached
	if delta == 0 {
		return nil
	}

	var payloadCode string
	if e.coreClient.Available() {
		bins, _ := e.coreClient.FetchNodeBins([]string{node.CoreNodeName})
		for _, b := range bins {
			if b.NodeName == node.CoreNodeName {
				payloadCode = b.PayloadCode
				break
			}
		}
	}
	if payloadCode == "" {
		return fmt.Errorf("could not resolve payload code for node %s — is Core reachable?", node.Name)
	}

	if e.inventoryDelta != nil {
		e.inventoryDelta.RecordBin(*runtime.ActiveBinID, payloadCode, delta, protocol.ReasonOperatorCorrection)
	}
	if err := e.db.UpdateProcessNodeUOP(nodeID, targetUOP); err != nil {
		return fmt.Errorf("update runtime UOP for node %s: %w", node.Name, err)
	}
	if e.inventoryDelta != nil {
		e.inventoryDelta.Flush()
	}

	log.Printf("admin_lineside: edited node %s bin=%d uop %d→%d delta=%+d",
		node.Name, *runtime.ActiveBinID, runtime.RemainingUOPCached, targetUOP, delta)
	return nil
}
