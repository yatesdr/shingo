package engine

import (
	"fmt"
	"log"
)

// RecordBinCount is the operator at the line correcting the count on the
// carrier in front of them, and telling Core.
//
// It is the missing half of a one-way channel. A count only ever travels with a
// DECLARATION — somebody, or some lifecycle event, deciding a number — and Core
// had two doors for declaring one downward (a clear or load, and the cycle
// count on the bins page) while the Edge had only release-shaped ones: the
// operator's number could reach Core if it rode an order release, and not
// otherwise. So the count that is most likely to be right, made by the person
// standing next to the carrier, had no way to reach the ledger.
//
// The declaration goes to Core FIRST. Core owns what a carrier is — its part,
// its claim, which generation of its life it is on — and it is the side that
// records the correction in the audit trail, clears the go-count-this flag, and
// broadcasts the corrected number to every station. If Core refuses (there is
// no carrier at that node, the count is negative, the payload has no capacity
// configured) the operator has to know, so nothing local is written and the
// error surfaces at the screen. A local write followed by a failed declaration
// would leave the two sides disagreeing with nobody aware.
//
// The local cache is then written from Core's reply rather than from the
// operator's input, so the two sides hold the same number by construction.
// Core's stamp comes back with it, which also picks up a generation this
// station had fallen behind on — free, because the write is happening anyway.
//
// This does NOT start a new generation. A count correction fixes a number
// inside the carrier's current life; a new generation is for a carrier that has
// been emptied and reloaded. Bumping here would retire a life that is still
// running and make this station's next report look stale — the exact failure
// the rest of this work exists to remove.
func (e *Engine) RecordBinCount(nodeID int64, actualUOP int, actor string) error {
	if actualUOP < 0 {
		return fmt.Errorf("count must be 0 or more")
	}
	node, _, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return err
	}
	if e.coreClient == nil {
		return fmt.Errorf("core API not configured")
	}
	counted, err := e.coreClient.RecordBinCount(node.CoreNodeName, actualUOP, actor)
	if err != nil {
		return fmt.Errorf("record count: %w", err)
	}

	// claim.ID is 0 for a synthesized Core-loader claim — pass nil, not a 0 FK.
	var claimIDPtr *int64
	if claim != nil && claim.ID != 0 {
		claimIDPtr = &claim.ID
	}
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.SetClaimCountAndEpoch(nodeID, claimIDPtr,
			counted.UOPRemaining, counted.BinID, counted.DeltaEpoch); err != nil {
			log.Printf("record_count: set runtime for node %d: %v", nodeID, err)
		}
	}
	log.Printf("record_count: node %s bin %d expected=%d actual=%d actor=%s discrepancy=%t",
		node.CoreNodeName, counted.BinID, counted.Expected, counted.UOPRemaining, actor, counted.Discrepancy)

	e.Events.Emit(Event{Type: EventUOPAdjusted, Payload: UOPAdjustedEvent{
		ProcessNodeID: nodeID,
		CoreNodeName:  node.CoreNodeName,
		BinID:         counted.BinID,
		NewRemaining:  counted.UOPRemaining,
		Actor:         actor,
	}})
	return nil
}
