package uop

import (
	"testing"

	"shingoedge/store/processes"
)

// TestComputeReleaseRemainingUOP_CaptureLineside_UnresolvableBin pins
// Issue 1's legacy-&0 safety net (lineside-buckets-investigation-
// 2026-05-18.md). When the operator picks PULL PARTS LINESIDE
// (DispositionCaptureLineside with a non-empty captures map) and the
// caller couldn't resolve a BinID for the released bin, the function
// returns &0 so Core's existing SyncOrClearForReleased(0) path wipes
// the manifest the way the pre-Item-6 dual-write did. Without this
// fallback the bin row keeps its original UOP after release — the
// silent-drop bug.
//
// The happy path (resolvedBinID > 0) still returns nil so the
// BinUOPDelta(capture_reduction) stream remains the single writer.
func TestComputeReleaseRemainingUOP_CaptureLineside_UnresolvableBin(t *testing.T) {
	t.Parallel()
	rt := &processes.RuntimeState{RemainingUOPCached: 96}
	disp := ReleaseDisposition{
		Mode:            DispositionCaptureLineside,
		LinesideCapture: map[string]int{"PART-A": 96},
	}

	t.Run("resolved_bin_id_zero_returns_zero_legacy_wipe", func(t *testing.T) {
		got := ComputeReleaseRemainingUOP(disp, rt, 0)
		if got == nil {
			t.Fatalf("got nil, want &0 — fallback wipe must fire when bin is unresolvable")
		}
		if *got != 0 {
			t.Errorf("got *%d, want *0", *got)
		}
	})

	t.Run("resolved_bin_id_positive_returns_nil_delta_path", func(t *testing.T) {
		got := ComputeReleaseRemainingUOP(disp, rt, 9001)
		if got != nil {
			t.Errorf("got *%d, want nil — delta path is the writer when bin resolves", *got)
		}
	})

	t.Run("empty_captures_keeps_release_empty_wipe", func(t *testing.T) {
		// RELEASE EMPTY (captures map empty) is unaffected — it has
		// always returned &0 regardless of bin resolution.
		emptyDisp := ReleaseDisposition{Mode: DispositionCaptureLineside}
		got := ComputeReleaseRemainingUOP(emptyDisp, rt, 0)
		if got == nil || *got != 0 {
			t.Errorf("got %v, want &0 — RELEASE EMPTY must keep the legacy wipe", got)
		}
		got2 := ComputeReleaseRemainingUOP(emptyDisp, rt, 9001)
		if got2 == nil || *got2 != 0 {
			t.Errorf("got %v, want &0 — RELEASE EMPTY must keep the legacy wipe even with resolved bin", got2)
		}
	})
}
