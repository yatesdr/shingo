package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
)

// AdminAdjustLinesideBucket is the engineer / team-leader override for a
// single lineside bucket, exposed via the edge "Lineside Buckets" admin
// page. Two ops:
//
//   - Edit qty (clearBucket=false): set the bucket to targetQty exactly.
//     Computes a signed delta against the current qty, emits a
//     LinesideBucketDelta with ReasonOperatorCorrectionBucket so Core's
//     lineside_buckets mirror tracks, and writes the new qty on edge
//     (deleting the row when the new qty is 0 — matches the
//     SetForReconcile contract).
//
//   - Clear bucket (clearBucket=true): targetQty is forced to 0. Same
//     wire-side mechanics; the bucket row is deleted on edge.
//
// The capture/drain pipeline is the normal source of bucket changes;
// this admin path is for unstuck scenarios where state has drifted
// (chip lingering on the HMI after operations the bucket layer didn't
// observe). Audit trail at Core: bin_uop_audit's bucket equivalent
// records source, station, and the delta with reason=operator_correction.
func (e *Engine) AdminAdjustLinesideBucket(bucketID int64, targetQty int, clearBucket bool) error {
	bucket, err := e.db.GetLinesideBucket(bucketID)
	if err != nil {
		return fmt.Errorf("get bucket %d: %w", bucketID, err)
	}

	if clearBucket {
		targetQty = 0
	}
	if targetQty < 0 {
		return fmt.Errorf("bucket qty cannot be negative")
	}

	if e.inventoryDelta != nil {
		// Resolve core_node_name for the wire envelope (Round-3 Obs 8).
		// The bucket row doesn't carry it, so look up from the process
		// node. Missing process_node is fatal for the admin path —
		// we'd send a delta Core would drop on validation anyway.
		node, err := e.db.GetProcessNode(bucket.NodeID)
		if err != nil || node == nil {
			return fmt.Errorf("resolve process_node %d for bucket %d: %w", bucket.NodeID, bucketID, err)
		}
		if err := e.inventoryDelta.AdjustBucket(
			bucket.NodeID, node.CoreNodeName, bucket.PairKey, bucket.StyleID, bucket.PartNumber,
			bucket.Qty, targetQty,
			protocol.ReasonOperatorCorrectionBucket,
		); err != nil {
			return fmt.Errorf("adjust bucket %d: %w", bucketID, err)
		}
	}

	op := "edited"
	if clearBucket {
		op = "cleared"
	}
	log.Printf("admin_lineside_bucket: %s bucket %d (node=%d style=%d part=%q): %d → %d delta=%+d",
		op, bucketID, bucket.NodeID, bucket.StyleID, bucket.PartNumber,
		bucket.Qty, targetQty, targetQty-bucket.Qty)
	return nil
}
