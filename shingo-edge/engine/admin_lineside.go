package engine

import (
	"fmt"
	"log"

	"shingoedge/store/lineside"
)

// AdminClearLinesideBucket is the engineer / team-leader Clear for one
// lineside pile, exposed on the Production page's lineside table: the row is
// deleted, active or stranded, and its level (now 0) is sent so Core's mirror
// loses it too.
//
// Unconditional: the delete does not depend on an inventory sink being wired.
// With no sink there is no mirror to tell, and the pile still goes.
//
// There is no qty edit. A pile exists only for parts a bin paid for; an edit
// upward would mint parts no bin gave up, and the capture and the drain are the
// only writers that move a pile's qty.
func (e *Engine) AdminClearLinesideBucket(bucketID int64) error {
	bucket, err := e.db.GetLinesideBucket(bucketID)
	if err != nil {
		return fmt.Errorf("get pile %d: %w", bucketID, err)
	}
	// The level is keyed by core_node_name. A node that cannot be resolved
	// leaves it empty and the accumulator resolves it from the node id at
	// flush (and drops the level loudly if it still cannot).
	var coreNodeName string
	if node, err := e.db.GetProcessNode(bucket.NodeID); err == nil && node != nil {
		coreNodeName = node.CoreNodeName
	}

	// The delete and the mark are one change to the seat for the lineside
	// report (countMu); PilesChanged takes the flush lock inside it.
	e.countMu.Lock()
	err = e.db.DeleteLinesideBucket(bucketID)
	if err == nil && e.inventoryDelta != nil {
		e.inventoryDelta.PilesChanged(lineside.Key{
			NodeID: bucket.NodeID, CoreNodeName: coreNodeName,
			PayloadCode: bucket.PayloadCode, State: bucket.State,
		})
	}
	e.countMu.Unlock()
	if err != nil {
		return fmt.Errorf("clear pile %d: %w", bucketID, err)
	}

	log.Printf("admin_lineside_bucket: cleared pile %d (node=%d core_node=%s payload=%q state=%s qty=%d)",
		bucketID, bucket.NodeID, coreNodeName, bucket.PayloadCode, bucket.State, bucket.Qty)
	return nil
}
