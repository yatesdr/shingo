// capture.go — operator release-click capture verb.
//
// CaptureToLineside owns the operator's release-click capture path:
// loops over disposition.LinesideCapture (qty per part), writes each
// captured qty to the active bucket, deactivates buckets for other
// styles on the node, emits the paired bin capture_reduction delta
// for the released bin.
//
// Atomic across the bin and bucket deltas — emit happens after the
// bucket DB writes succeed, so a partial DB failure doesn't ship
// inconsistent envelopes. (The accumulator doesn't fire deltas to
// Core until the next flush; the engine's release-click path
// triggers a flush separately at operator_release.go:374.)
//
// Moved from engine/operator_release.go's captureLinesideOnRelease in
// Phase 3a.
package uop

import (
	"fmt"
	"log"

	"shingo/protocol"
)

// CaptureEvent carries the release-click capture context. Engine
// populates from the resolved order + disposition + node state at
// release time.
type CaptureEvent struct {
	NodeID  int64
	StyleID int64 // the to-style claim's style (target style after release)
	PairKey string

	// CoreNodeName is the cross-system identifier the wire envelope
	// uses (Round-3 Obs 8). Engine populates from the process node
	// row that drives the capture.
	CoreNodeName string

	// Disposition carries Mode + LinesideCapture map + other operator
	// intent fields. Only Mode == DispositionCaptureLineside drives
	// the bucket capture loop; other modes still call this verb to
	// get the deactivate-other-styles side effect.
	Disposition ReleaseDisposition

	// BinID + PayloadCode identify the bin being released (source of
	// the capture_reduction delta). BinID == 0 skips the bin delta.
	// PayloadCode is the order's recorded payload (not the to-style
	// template) — see operator_release.go's comment on Core's
	// payload-mismatch validation.
	BinID       int64
	PayloadCode string

	// SuppressBinDelta is true for the supply leg (Order A) of a
	// two-robot swap. The supply bin is fresh and had nothing pulled
	// from it; emitting capture_reduction would corrupt the
	// authoritative count.
	SuppressBinDelta bool
}

// CaptureToLineside performs the operator's release-click capture:
//
//  1. For each non-zero (part, qty) in disposition.LinesideCapture:
//     write the qty into the active bucket via
//     bucketStore.CaptureLinesideBucket; emit
//     LinesideBucketDelta(capture_fill, +qty) per part.
//  2. Always (regardless of disposition mode): deactivate other
//     styles on this node so future drains resolve to the right
//     active bucket.
//  3. If capturedTotal > 0 AND !SuppressBinDelta AND BinID > 0:
//     emit BinUOPDelta(capture_reduction, -capturedTotal) for the
//     released bin.
//
// Returns capturedTotal so the caller can record it for logging /
// auditing. Errors short-circuit — a failure during bucket capture
// returns without emitting the bin reduction (preserving the
// invariant that capture_reduction's magnitude matches the sum of
// capture_fill emissions).
func (m *Mutator) CaptureToLineside(ev CaptureEvent) (capturedTotal int, err error) {
	if ev.Disposition.Mode == DispositionCaptureLineside {
		for part, qty := range ev.Disposition.LinesideCapture {
			if qty <= 0 || part == "" {
				continue
			}
			if _, err := m.buckets.CaptureLinesideBucket(ev.NodeID, ev.PairKey, ev.StyleID, part, qty); err != nil {
				return capturedTotal, fmt.Errorf("capture lineside bucket (node=%d style=%d part=%s): %w",
					ev.NodeID, ev.StyleID, part, err)
			}
			// PayloadCode (UOP-threshold replenishment) — capture event
			// carries the bin's payload, which is the same payload the
			// captured parts belong to. Core's SystemUOPForPayload sums
			// bins + buckets keyed on this.
			m.acc.recordBucket(ev.NodeID, ev.CoreNodeName, ev.PairKey, ev.StyleID, part, ev.PayloadCode, qty, protocol.ReasonCaptureFill)
			capturedTotal += qty
		}
	}

	if err := m.buckets.DeactivateOtherLinesideStyles(ev.NodeID, ev.StyleID); err != nil {
		return capturedTotal, fmt.Errorf("deactivate other lineside styles on node %d: %w", ev.NodeID, err)
	}

	if capturedTotal > 0 && !ev.SuppressBinDelta {
		if ev.BinID > 0 {
			m.acc.recordBin(ev.BinID, ev.PayloadCode, -capturedTotal, protocol.ReasonCaptureReduction)
		} else {
			// Loud diagnostic. The capture path used to silently drop
			// the capture_reduction here when the caller couldn't
			// resolve a bin id, and the bug shipped to a plant
			// (lineside-buckets-investigation-2026-05-18.md). The
			// release-path now falls back to a legacy RemainingUOP=&0
			// wipe in this case, but a recurrence must remain
			// visible in operator logs instead of vanishing.
			log.Printf("ERROR: uop capture: capture_reduction skipped (BinID=0) node=%d style=%d payload=%q captured_total=%d disposition=%q",
				ev.NodeID, ev.StyleID, ev.PayloadCode, capturedTotal, ev.Disposition.Mode)
		}
	}
	return capturedTotal, nil
}
