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

	delta := targetQty - bucket.Qty
	if delta != 0 && e.inventoryDelta != nil {
		e.inventoryDelta.RecordBucket(bucket.NodeID, bucket.PairKey, bucket.StyleID, bucket.PartNumber,
			delta, protocol.ReasonOperatorCorrectionBucket)
	}

	if err := e.db.SetLinesideBucketForReconcile(bucket.NodeID, bucket.PairKey, bucket.StyleID, bucket.PartNumber, targetQty); err != nil {
		return fmt.Errorf("update bucket %d: %w", bucketID, err)
	}

	if e.inventoryDelta != nil {
		e.inventoryDelta.Flush()
	}

	op := "edited"
	if clearBucket {
		op = "cleared"
	}
	log.Printf("admin_lineside_bucket: %s bucket %d (node=%d style=%d part=%q): %d → %d delta=%+d",
		op, bucketID, bucket.NodeID, bucket.StyleID, bucket.PartNumber,
		bucket.Qty, targetQty, delta)
	return nil
}
