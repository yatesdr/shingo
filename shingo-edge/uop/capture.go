// capture.go — operator release-click capture verb.
//
// CaptureToLineside owns the operator's release-click capture path: loops
// over disposition.LinesideCapture (qty per part), adds each captured qty to
// the node's ACTIVE pile of that part, marks each pile's level dirty, and
// records the paired bin capture_reduction delta for the released bin.
//
// A pull is a transfer (bin -> bench), not consumption: the bin loses what the
// pile gains, in the same call. The supply leg of a two-robot swap has no bin
// that gave anything up, so it makes no pile at all.
//
// Nothing reaches Core until the next flush; the engine's release-click path
// triggers one separately.
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
	NodeID int64

	// CoreNodeName is the cross-system identifier the pile level is keyed
	// by at Core. Engine populates from the process node row that drives the
	// capture.
	CoreNodeName string

	// Disposition carries Mode + LinesideCapture map + other operator
	// intent fields. Only Mode == DispositionCaptureLineside captures.
	Disposition ReleaseDisposition

	// BinID + PayloadCode identify the bin being released (source of
	// the capture_reduction delta). BinID == 0 skips the bin delta.
	// PayloadCode is the order's recorded payload (not the to-style
	// template) — see operator_release.go's comment on Core's
	// payload-mismatch validation.
	BinID       int64
	PayloadCode string

	// BinEpoch is the released bin's load-lifecycle epoch — threaded
	// through to recordBin so the capture_reduction emission carries
	// the right generation in the BinUOPDelta envelope. Caller
	// resolves from the runtime bin-state at release time.
	BinEpoch int64

	// SuppressBinDelta is true for the supply leg (Order A) of a
	// two-robot swap. The supply bin is fresh and had nothing pulled
	// from it, so the leg captures nothing: no pile (a pile exists only
	// for parts a bin paid for) and no capture_reduction.
	SuppressBinDelta bool
}

// CaptureToLineside performs the operator's release-click capture:
//
//  1. Supply leg (SuppressBinDelta): nothing. No bin paid for the parts.
//  2. For each non-zero (part, qty) in disposition.LinesideCapture: add qty
//     to the node's active pile of the part and mark its level dirty. A
//     stranded pile of the part is left as it is.
//  3. If capturedTotal > 0 AND BinID > 0: record BinUOPDelta(
//     capture_reduction, -capturedTotal) for the released bin.
//
// Returns capturedTotal. A failure writing a pile returns without the bin
// reduction, so capture_reduction's magnitude always matches the piles'
// gain.
func (m *Mutator) CaptureToLineside(ev CaptureEvent) (capturedTotal int, err error) {
	if ev.SuppressBinDelta || ev.Disposition.Mode != DispositionCaptureLineside {
		return 0, nil
	}
	for part, qty := range ev.Disposition.LinesideCapture {
		if qty <= 0 || part == "" {
			continue
		}
		if _, err := m.buckets.CaptureLinesideBucket(ev.NodeID, part, qty); err != nil {
			return capturedTotal, fmt.Errorf("capture lineside pile (node=%d part=%s): %w",
				ev.NodeID, part, err)
		}
		// The CHIP's payload, not the bin's. They are the same on a
		// single-payload node and different the moment an operator pulls
		// one of several allowed payloads off a bin holding another.
		m.acc.markBucket(ev.NodeID, ev.CoreNodeName, part, protocol.LinesideBucketActive, 0)
		capturedTotal += qty
	}

	if capturedTotal > 0 {
		if ev.BinID > 0 {
			m.acc.recordBin(ev.BinID, ev.PayloadCode, -capturedTotal, protocol.ReasonCaptureReduction, ev.BinEpoch)
		} else {
			// Loud diagnostic. The capture path used to silently drop
			// the capture_reduction here when the caller couldn't
			// resolve a bin id, and the bug shipped to a plant
			// (lineside-buckets-investigation-2026-05-18.md). The
			// release-path now falls back to a legacy RemainingUOP=&0
			// wipe in this case, but a recurrence must remain
			// visible in operator logs instead of vanishing.
			log.Printf("ERROR: uop capture: capture_reduction skipped (BinID=0) node=%d payload=%q captured_total=%d disposition=%q",
				ev.NodeID, ev.PayloadCode, capturedTotal, ev.Disposition.Mode)
		}
	}
	return capturedTotal, nil
}
