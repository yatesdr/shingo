package engine

import (
	"fmt"
	"sort"
	"strings"

	"shingoedge/store"
	"shingoedge/uop"
)

// captureOnce returns the disposition the capture step should apply for this
// order: the operator's per-part quantities less what an earlier attempt at the
// same release already captured, floored at zero. The same quantities apply
// nothing; larger ones apply only the extra. prior is what had been captured
// (nil when nothing had), for recordCapture.
//
// Only a release that carries captures reads the record: one read. Every other
// release — RELEASE EMPTY, SEND PARTIAL, the supply leg — pays nothing.
//
// The record is keyed by the order, not the bin: the bin a release resolves
// can differ between two attempts (BinAtLineside answering differently, the
// runtime pointer moving and the epoch falling to 0), and keying by it would
// read the retry as a new capture.
func (e *Engine) captureOnce(orderID int64, disp ReleaseDisposition, isSupply bool) (ReleaseDisposition, *store.ReleaseCaptureRecord, error) {
	if isSupply || disp.Mode != uop.DispositionCaptureLineside || len(disp.LinesideCapture) == 0 {
		return disp, nil, nil
	}
	prior, err := e.db.GetReleaseCapture(orderID)
	if err != nil {
		return disp, nil, fmt.Errorf("order %d: could not read what this release already captured (%w)", orderID, err)
	}
	if prior == nil {
		return disp, nil, nil
	}
	diff := make(map[string]int, len(disp.LinesideCapture))
	for part, qty := range disp.LinesideCapture {
		if extra := qty - prior.Parts[part]; extra > 0 {
			diff[part] = extra
		}
	}
	out := disp
	out.LinesideCapture = diff
	return out, prior, nil
}

// recordCapture writes what this order has now captured — per part, the larger
// of the earlier record and this attempt — and logs one line saying what was
// applied and what was skipped as a repeat.
//
// Written after the capture, not in its transaction (the capture's writes live
// in the uop mutator). A crash between the two leaves the capture unrecorded,
// which re-applies it once on a retry: the defect this closes, reduced to a
// crash window.
func (e *Engine) recordCapture(orderID int64, requested, applied ReleaseDisposition, prior *store.ReleaseCaptureRecord,
	binID, epoch int64, isSupply bool) {
	if isSupply || requested.Mode != uop.DispositionCaptureLineside || len(requested.LinesideCapture) == 0 {
		return
	}
	merged := map[string]int{}
	if prior != nil {
		for part, qty := range prior.Parts {
			merged[part] = qty
		}
	}
	for part, qty := range requested.LinesideCapture {
		if qty > merged[part] {
			merged[part] = qty
		}
	}
	if err := e.db.PutReleaseCapture(orderID, store.ReleaseCaptureRecord{BinID: binID, Epoch: epoch, Parts: merged}); err != nil {
		e.logRelease("order=%d bin=%d capture: applied but NOT recorded (%v) — a retry of this release will capture again", orderID, binID, err)
	}
	parts := make([]string, 0, len(requested.LinesideCapture))
	for part := range requested.LinesideCapture {
		parts = append(parts, part)
	}
	sort.Strings(parts)
	var lines []string
	for _, part := range parts {
		asked, got := requested.LinesideCapture[part], applied.LinesideCapture[part]
		switch {
		case prior == nil:
			lines = append(lines, fmt.Sprintf("%s=%d applied", part, asked))
		case got == 0:
			lines = append(lines, fmt.Sprintf("%s=%d skipped as a repeat (already %d)", part, asked, prior.Parts[part]))
		default:
			lines = append(lines, fmt.Sprintf("%s=%d applied %d (already %d)", part, asked, got, prior.Parts[part]))
		}
	}
	e.logRelease("order=%d bin=%d epoch=%d capture: %s", orderID, binID, epoch, strings.Join(lines, ", "))
}
