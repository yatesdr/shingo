// release.go — pure-data types and functions for the operator release path.
//
// ReleaseDisposition is the operator's release-time intent (CaptureLineside,
// SendPartialBack, ReleaseUnderpack) plus the metadata Core needs to audit
// the operator's override against the system's suggested values.
//
// ComputeReleaseRemainingUOP and BuildProtocolDisposition are the two pure
// functions that translate an Edge-side ReleaseDisposition into the wire
// shape Core consumes. They live in uop/ rather than engine/ because the
// disposition is conceptually a UOP-state-change request, not engine
// orchestration. Phase 3a's Capturer.CaptureToLineside verb takes a
// ReleaseDisposition by value and consumes it internally.
//
// Moved from shingo-edge/engine/operator_release.go in Phase 2.
package uop

import (
	"shingo/protocol"
	"shingoedge/store/processes"
)

// ReleaseDispositionMode controls how a release action interacts with the
// bin's manifest at Core.
type ReleaseDispositionMode string

const (
	// DispositionCaptureLineside is the default operator-confirmed-empty
	// disposition: any pulled parts are captured as lineside buckets and
	// Core clears the bin's manifest (remaining_uop=0).
	DispositionCaptureLineside ReleaseDispositionMode = "capture_lineside"
	// DispositionSendPartialBack returns the partially-consumed bin to the
	// supermarket with its current UOP count. No bucket capture; Core syncs
	// uop_remaining and preserves the manifest.
	DispositionSendPartialBack ReleaseDispositionMode = "send_partial_back"
	// DispositionReleaseUnderpack — operator declares the bin is
	// physically empty before the tracked count reaches zero (e.g.
	// bin labeled 1200 actually held 1190; cell starves at
	// runtime=10). Wire shape mirrors RELEASE EMPTY (RemainingUOP =
	// &0, manifest cleared); the disposition tag carries the
	// "physical inventory was less than tracked" signal forward so
	// Core's audit row records released_underpack instead of
	// released_empty. Forensics trend the missing-inventory delta
	// from suggested_uop - after_uop in bin_uop_audit.
	DispositionReleaseUnderpack ReleaseDispositionMode = "release_underpack"
)

// ReleaseDisposition carries the operator's release-time intent from the HTTP
// handler down through ReleaseOrderWithLineside to the order manager. The
// zero value (all fields zero/nil) is the backward-compat default — no
// manifest change at Core.
//
// Phase 0b adds the operator-override audit fields. The HTTP handler
// captures whichever values the system would have suggested at modal-open
// time and threads them through here so Core can record divergences:
//
//   - LinesideCaptureSuggested: per-part baseline for the capture path
//     (chip pre-population came from runtime.RemainingUOPCached / manifest qtys).
//   - PartialCount, PartialCountSuggested: operator-entered count and the
//     pre-populated baseline for the SEND PARTIAL BACK path. PartialCount
//     supersedes runtime.RemainingUOPCached for the wire when set.
//
// Suggested fields are nil-safe — legacy HTTP clients that don't ship
// the override-aware body just don't populate them, and Core writes no
// override audit row.
type ReleaseDisposition struct {
	Mode                     ReleaseDispositionMode
	LinesideCapture          map[string]int // qty per part — only valid when Mode == DispositionCaptureLineside
	LinesideCaptureSuggested map[string]int // system-suggested per-part qty at modal-open (Phase 0b)
	PartialCount             *int           // operator-entered count for SEND PARTIAL BACK (Phase 0b); supersedes runtime when set
	PartialCountSuggested    *int           // system-suggested count at modal-open for SEND PARTIAL BACK (Phase 0b)
	CalledBy                 string         // operator identity for audit
}

// ComputeReleaseRemainingUOP returns the *int that should be threaded to
// orderMgr.ReleaseOrder as the remaining_uop value, based on the disposition.
//
// Returns:
//   - &0 for DispositionCaptureLineside (operator-confirmed empty, manifest cleared)
//     IF there are no captures; nil if captures are present (capture_reduction
//     delta is the writer in that case).
//   - The operator-entered or runtime-cache value for DispositionSendPartialBack.
//   - &0 for DispositionReleaseUnderpack (bin physically empty before tracked
//     count reached zero).
//   - nil for unrecognized / zero-value Mode (no manifest action).
//
// SendPartialBack source priority (Phase 0b):
//  1. disp.PartialCount (operator-entered via the keypad) when set and >0.
//     Per the SME contract the operator's count is ground truth.
//  2. runtime.RemainingUOPCached when >0. Fallback for legacy HTTP clients that
//     don't ship the override-aware body shape.
//  3. &0 otherwise — no positive baseline to preserve, declare empty.
func ComputeReleaseRemainingUOP(disp ReleaseDisposition, runtime *processes.RuntimeState) *int {
	switch disp.Mode {
	case DispositionCaptureLineside:
		// RELEASE EMPTY (no captures, just operator-confirmed empty)
		// keeps the legacy &0 path: Core's SyncOrClearForReleased(0)
		// wipes the manifest and audits as released_empty.
		//
		// PULL PARTS LINESIDE (with captures) returns nil — the
		// BinUOPDelta(capture_reduction) is now the single writer to
		// bins.uop_remaining; Core's capture-reduction-to-zero
		// trigger handles the manifest clear and audits as
		// released_capture_empty. Item 6 of the bin-as-truth refactor
		// retires the dual-write at this site.
		if len(disp.LinesideCapture) == 0 {
			zero := 0
			return &zero
		}
		return nil
	case DispositionSendPartialBack:
		if disp.PartialCount != nil && *disp.PartialCount > 0 {
			v := *disp.PartialCount
			return &v
		}
		if runtime != nil && runtime.RemainingUOPCached > 0 {
			v := runtime.RemainingUOPCached
			return &v
		}
		// Non-positive runtime UOP: nothing to preserve, fall through to empty.
		zero := 0
		return &zero
	case DispositionReleaseUnderpack:
		// Same wire shape as RELEASE EMPTY — bin physically empty,
		// manifest cleared at Core. The audit-tag distinction lives
		// in the disposition Kind (released_underpack) which
		// BuildProtocolDisposition threads to Core.
		zero := 0
		return &zero
	default:
		// "" / unknown mode → backward-compat: no manifest action.
		return nil
	}
}

// BuildProtocolDisposition translates the Edge-side ReleaseDisposition
// into the wire-shape protocol.UOPDisposition. Phase 0b: rides alongside
// the legacy RemainingUOP field on OrderRelease and carries the
// suggested baselines for Core's override audit.
//
// Mode mapping:
//
//   - DispositionCaptureLineside with non-empty captures → DispositionPullParts
//   - DispositionCaptureLineside with no captures → DispositionReleaseEmpty
//     (matches the current "RELEASE EMPTY" UI body shape: capture_lineside
//     with an empty qty_by_part map)
//   - DispositionSendPartialBack → DispositionReleasePartial. Count comes
//     from PartialCount when set, else from runtime.RemainingUOPCached — same
//     priority as ComputeReleaseRemainingUOP.
//   - "" (zero Mode) → nil. Legacy callers ship no Disposition.
//
// Returns nil when there's nothing meaningful to ship (preserves the
// "no manifest action" semantic).
func BuildProtocolDisposition(disp ReleaseDisposition, runtime *processes.RuntimeState) *protocol.UOPDisposition {
	switch disp.Mode {
	case DispositionCaptureLineside:
		// Empty captures map === RELEASE EMPTY in the current UI.
		if len(disp.LinesideCapture) == 0 {
			return &protocol.UOPDisposition{Kind: protocol.DispositionReleaseEmpty}
		}
		return &protocol.UOPDisposition{
			Kind:              protocol.DispositionPullParts,
			Captures:          disp.LinesideCapture,
			CapturesSuggested: disp.LinesideCaptureSuggested,
		}
	case DispositionSendPartialBack:
		d := &protocol.UOPDisposition{Kind: protocol.DispositionReleasePartial}
		switch {
		case disp.PartialCount != nil && *disp.PartialCount > 0:
			d.Count = *disp.PartialCount
		case runtime != nil && runtime.RemainingUOPCached > 0:
			d.Count = runtime.RemainingUOPCached
		}
		d.CountSuggested = disp.PartialCountSuggested
		return d
	case DispositionReleaseUnderpack:
		// CountSuggested carries the system's expected count (the
		// runtime cache at click time). Core's bin_uop_audit row
		// will pick up before_uop = current bins.uop_remaining,
		// after_uop = 0; the suggested_uop - after_uop gap is the
		// missing-inventory delta forensics read.
		d := &protocol.UOPDisposition{Kind: protocol.DispositionReleaseUnderpack}
		if runtime != nil {
			v := runtime.RemainingUOPCached
			d.CountSuggested = &v
		}
		return d
	default:
		return nil
	}
}
