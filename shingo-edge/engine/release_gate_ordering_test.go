package engine

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
)

// ---------------------------------------------------------------------------
// A refused RELEASE must not have shipped the departing bin's paperwork.
//
// ReleaseStagedOrders called produceIngestAtRelease — which ships the ingest
// manifest to Core, clears active_bin_id and zeroes remaining_uop_cached, the
// point its own comment calls the start of the hold-and-replay window — and
// only then asked whether the release was allowed at all. The refusal is
// advisory: "the other robot has not cleared the press yet, click again". By
// then the press's count had been handed to a bin that has not left, the slot
// had been zeroed, and the press was still making parts into it.
//
// The existing gate tests call refusePlacingLegWhileSiblingPending directly, so
// nothing saw the ordering. These drive the whole function.
// ---------------------------------------------------------------------------

// ingestManifestsQueued counts the ingest-manifest envelopes sitting in the
// outbox — the observable half of "the paperwork shipped".
func ingestManifestsQueued(t *testing.T, db *store.DB) int {
	t.Helper()
	msgs, err := db.ListPendingOutbox(200)
	testutil.MustNoErr(t, err, "ListPendingOutbox")
	n := 0
	for _, m := range msgs {
		if m.MsgType == protocol.TypeOrderIngest {
			n++
		}
	}
	return n
}

func remainingUOP(t *testing.T, db *store.DB, nodeID int64) int {
	t.Helper()
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime")
	if rt == nil {
		t.Fatal("no runtime row")
	}
	return rt.RemainingUOPCached
}

// TestReleaseStagedOrders_HeldReleaseShipsNoPaperwork is the F3 regression.
//
// The supply leg is staged and the evac has not run, so the collision guard
// holds the release. Nothing about the departing bin may have moved.
func TestReleaseStagedOrders_HeldReleaseShipsNoPaperwork(t *testing.T) {
	t.Parallel()
	eng, nodeID, _, _ := seedSwapPairAt(t,
		protocol.SwapModeTwoRobotPressIndex, protocol.StatusQueued, protocol.StatusStaged)

	// The press has counted parts into the bin that is on it.
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntime(nodeID, nil, 42), "seed a live count")
	before := remainingUOP(t, eng.db, nodeID)
	if before != 42 {
		t.Fatalf("seeded remaining = %d, want 42", before)
	}
	manifestsBefore := ingestManifestsQueued(t, eng.db)

	err := eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{CalledBy: "operator:test"})
	if err == nil {
		t.Fatal("want the collision guard to hold this release")
	}
	var notReady *SwapPairNotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("want a *SwapPairNotReadyError; got %T (%v)", err, err)
	}

	if got := ingestManifestsQueued(t, eng.db); got != manifestsBefore {
		t.Errorf("a HELD release queued %d ingest manifest(s) — the bin has not left the press, "+
			"so its count has not been handed to anyone; the gate must run before the paperwork",
			got-manifestsBefore)
	}
	if got := remainingUOP(t, eng.db, nodeID); got != before {
		t.Errorf("remaining_uop_cached = %d after a HELD release, want %d unchanged — zeroing it "+
			"starts the hold-and-replay window for a bin that is still on the press and still filling",
			got, before)
	}
}

// TestReleaseStagedOrders_AllowedReleaseStillShipsPaperwork is the control. The
// hoist must not turn into "the paperwork never runs": an ordinary release —
// both legs releasable, nothing to collide with — still stamps the manifest and
// starts the window.
func TestReleaseStagedOrders_AllowedReleaseStillShipsPaperwork(t *testing.T) {
	t.Parallel()
	eng, nodeID, _, _ := seedSwapPairAt(t,
		protocol.SwapModeTwoRobotPressIndex, protocol.StatusStaged, protocol.StatusStaged)

	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntime(nodeID, nil, 42), "seed a live count")
	manifestsBefore := ingestManifestsQueued(t, eng.db)

	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{CalledBy: "operator:test"}),
		"release a pair with nothing to collide with")

	if got := ingestManifestsQueued(t, eng.db); got <= manifestsBefore {
		t.Error("an allowed produce release queued no ingest manifest — the departing bin's count " +
			"has to reach Core, and moving the gate above the paperwork must not skip it")
	}
	if got := remainingUOP(t, eng.db, nodeID); got != 0 {
		t.Errorf("remaining_uop_cached = %d after an allowed release, want 0 — the count now belongs "+
			"to the departing bin and the hold-and-replay window starts here", got)
	}
}
