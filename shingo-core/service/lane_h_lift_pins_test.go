//go:build docker

package service

import (
	"errors"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// Lane H, DECISIONS D11 — the generation ends at the lift, not the release.
// Unit pins for Core's half of both mechanisms: (i) the release's stream fence
// and the old-Edge bump it keeps, (ii) the applier's time rule at the lift.
// The two-book pins are integration/scenarios/carrier_handoff*_test.go.

func ledgerOps(t *testing.T, db *store.DB, binID int64, op string) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT count(*) FROM bin_uop_ledger WHERE bin_id=$1 AND op=$2`,
		binID, op).Scan(&n), "count ledger ops")
	return n
}

func liftNetDelta(binID, epoch, seq int64, delta int, net int64, start time.Time) *protocol.BinUOPDelta {
	return &protocol.BinUOPDelta{
		BinID: binID, PayloadCode: "PART-A", Delta: delta, Reason: protocol.ReasonConsumeTick,
		SequenceID: seq, Epoch: epoch, WindowStart: start, WindowEnd: start.Add(time.Second), Net: &net,
	}
}

// TestRelease_FencedReleaseSkipsALatePreReleaseDelta — D11 (i), DESIGN-H
// §11(b). The station flushed seq 2 before the release and the drainer
// delivers it after; the release declared its count as of seq 2 plus one
// unflushed tick. Without the fence, seq 2 lands on the declared count (a
// double subtraction). With it: no bump, seq 2 is at or below high water and
// skipped, and the next window applies only the tick after the declaration.
func TestRelease_FencedReleaseSkipsALatePreReleaseDelta(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	manifest := announcingService(db)
	deltas := NewInventoryDeltaService(db, manifest, EpochAnnounce{Topic: announceTopic, CoreStation: "core.test"})

	bin := createTestBin(t, db, sd.LineNode.ID, "BIN-FENCE", "PART-A", 100)
	order := createTestOrder(t, db, sd.StorageNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)
	e := binEpoch(t, db, bin.ID)
	now := time.Now().UTC()

	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(pinStation, liftNetDelta(bin.ID, e, 1, -2, -2, now)), "seq 1")
	// seq 2 (-3, net -5) is flushed but not delivered; one tick is unflushed.
	// The operator declares 94 (100 - 2 - 3 - 1).
	declared := 94
	testutil.MustNoErr(t, manifest.SyncOrClearForReleasedFenced(bin.ID, order.ID, &declared,
		protocol.DispositionReleasePartial, "operator",
		&ReleaseFence{Station: pinStation, ReleaseFence: protocol.ReleaseFence{BinID: bin.ID, Epoch: e, AsOfSeq: 2, AsOfNet: -6, AsOfAt: now.Add(2 * time.Second)}}),
		"fenced release")
	if got := binEpoch(t, db, bin.ID); got != e {
		t.Fatalf("a fenced release moved delta_epoch %d -> %d; the generation ends at the lift", e, got)
	}
	if adj := outboxAdjustments(t, db, bin.ID); len(adj) != 0 {
		t.Errorf("a fenced release announced %d generation(s); it bumps nothing, so it announces nothing", len(adj))
	}

	err := deltas.ApplyBinUOPDelta(pinStation, liftNetDelta(bin.ID, e, 2, -3, -5, now.Add(time.Second)))
	if !errors.Is(err, ErrInventoryDeltaSkipped) {
		t.Errorf("late pre-release seq 2: err = %v, want skipped (at or below the fence)", err)
	}
	if got := binRemaining(t, db, bin.ID); got != declared {
		t.Errorf("after the late pre-release delta uop_remaining = %d, want %d: its count is in the declaration", got, declared)
	}
	// seq 3 carries the unflushed tick and one tick after the release: net -8.
	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(pinStation, liftNetDelta(bin.ID, e, 3, -2, -8, now.Add(2*time.Second))), "seq 3")
	if got := binRemaining(t, db, bin.ID); got != declared-2 {
		t.Errorf("after seq 3 uop_remaining = %d, want %d: only the ticks after the fence (net -8 against -6) apply", got, declared-2)
	}
	if n := ledgerOps(t, db, bin.ID, "stale_epoch_dropped"); n != 0 {
		t.Errorf("stale_epoch_dropped = %d, want 0", n)
	}
}

// TestRelease_UnfencedReleaseStillBumps — D11 (i), mixed-version safety. A
// release from an Edge that predates the fence carries none, and Core keeps
// today's bump and announcement in the release's transaction: that Edge has
// no other way to stop its pre-release deltas landing on the declaration.
// A fence for a generation the bin is no longer at counts as none.
func TestRelease_UnfencedReleaseStillBumps(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	manifest := announcingService(db)

	for _, tc := range []struct {
		label string
		fence *ReleaseFence
	}{
		{"BIN-NOFENCE", nil},
		{"BIN-OLDFENCE", &ReleaseFence{Station: pinStation, ReleaseFence: protocol.ReleaseFence{Epoch: -1}}},
	} {
		bin := createTestBin(t, db, sd.LineNode.ID, tc.label, "PART-A", 50)
		order := createTestOrder(t, db, sd.StorageNode.ID)
		claimBinForTest(t, db, bin.ID, order.ID)
		e := binEpoch(t, db, bin.ID)
		if tc.fence != nil {
			tc.fence.BinID = bin.ID
		}
		n := 40
		testutil.MustNoErr(t, manifest.SyncOrClearForReleasedFenced(bin.ID, order.ID, &n,
			protocol.DispositionReleasePartial, "operator", tc.fence), "release")
		if got := binEpoch(t, db, bin.ID); got != e+1 {
			t.Errorf("%s: release left delta_epoch %d -> %d, want a bump (no usable fence)", tc.label, e, got)
		}
		if adj := outboxAdjustments(t, db, bin.ID); len(adj) != 1 || adj[0].Epoch != e+1 {
			t.Errorf("%s: announcements = %+v, want one at epoch %d", tc.label, adj, e+1)
		}
	}
}

// TestApplier_AfterTheLiftATimeRule — D11 (ii), R4. The lift of a streamed
// carrier ends its generation and stamps lifted_at. A window of the ended
// generation that started before the lift applies onto the carrier (the count
// is continuous across the lift); one that started at or after it is refused
// as after_lift: one ledger row, no exception, no anomaly, no refresh. A window
// at the new generation that starts after the lift, while the carrier is in
// transit, is refused the same way.
func TestApplier_AfterTheLiftATimeRule(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	manifest := announcingService(db)
	binSvc := NewBinService(db, manifest)
	deltas := NewInventoryDeltaService(db, manifest, EpochAnnounce{Topic: announceTopic, CoreStation: "core.test"})

	bin := createTestBin(t, db, sd.LineNode.ID, "BIN-LIFT", "PART-A", 60)
	order := createTestOrder(t, db, sd.StorageNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)
	e := binEpoch(t, db, bin.ID)
	beforeLift := time.Now().UTC().Add(-time.Second)

	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(pinStation, liftNetDelta(bin.ID, e, 1, -1, -1, beforeLift)), "seq 1")
	if _, err := binSvc.Lift(bin.ID, PickupNotice{Station: pinStation, OrderUUID: "lift-pin", OrderID: order.ID, Location: sd.LineNode.Name}); err != nil {
		t.Fatalf("lift: %v", err)
	}
	if got := binEpoch(t, db, bin.ID); got != e+1 {
		t.Fatalf("the lift of a seat carrier left delta_epoch %d -> %d, want a bump", e, got)
	}
	if n := ledgerOps(t, db, bin.ID, "lifted"); n != 1 {
		t.Errorf("lifted ledger rows = %d, want 1 (the binding boundary)", n)
	}
	var liftedAt time.Time
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT lifted_at FROM bins WHERE id=$1`, bin.ID).Scan(&liftedAt), "lifted_at")

	// Pre-lift window, delivered after the lift: applies at E.
	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(pinStation, liftNetDelta(bin.ID, e, 2, -2, -3, beforeLift.Add(500*time.Millisecond))),
		"a window that started before the lift")
	if got := binRemaining(t, db, bin.ID); got != 57 {
		t.Errorf("after the pre-lift window uop_remaining = %d, want 57", got)
	}
	// Post-lift windows: refused, at the ended generation and at the new one.
	for i, d := range []*protocol.BinUOPDelta{
		liftNetDelta(bin.ID, e, 3, -4, -7, liftedAt.Add(time.Millisecond)),
		liftNetDelta(bin.ID, e+1, 1, -1, -1, liftedAt.Add(2*time.Millisecond)),
	} {
		if err := deltas.ApplyBinUOPDelta(pinStation, d); !errors.Is(err, ErrInventoryDeltaSkipped) {
			t.Errorf("post-lift window %d: err = %v, want refused", i, err)
		}
	}
	if got := binRemaining(t, db, bin.ID); got != 57 {
		t.Errorf("after the post-lift windows uop_remaining = %d, want 57: a window after the lift is not the carrier's", got)
	}
	if n := ledgerOps(t, db, bin.ID, "after_lift"); n != 2 {
		t.Errorf("after_lift rows = %d, want 2", n)
	}
	if n := ledgerOps(t, db, bin.ID, "stale_epoch_dropped"); n != 0 {
		t.Errorf("stale_epoch_dropped = %d, want 0", n)
	}
	var exceptions int
	var anomaly bool
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT count(*) FROM bin_uop_exception WHERE bin_id=$1 AND kind <> 'boundary'`,
		bin.ID).Scan(&exceptions), "count exceptions")
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT anomaly_at IS NOT NULL FROM bins WHERE id=$1`, bin.ID).Scan(&anomaly), "anomaly")
	if exceptions != 0 || anomaly {
		t.Errorf("after_lift wrote %d exception rows, anomaly %t; want none: the rule working is not an anomaly", exceptions, anomaly)
	}
	var picked int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT count(*) FROM outbox WHERE msg_type=$1 AND convert_from(payload, 'UTF8') LIKE '%lift-pin%'`,
		"data."+protocol.SubjectBinPickedUp).Scan(&picked), "count pickups")
	if picked != 1 {
		t.Errorf("pickup messages in the outbox = %d, want 1, written with the lift", picked)
	}
	if adj := outboxAdjustments(t, db, bin.ID); len(adj) != 0 {
		t.Errorf("the lift broadcast %d uop_adjustment(s); its announcement is the pickup (named exception)", len(adj))
	}

	// The arrival ends the lift's window.
	if _, err := binSvc.ApplyArrival(bin.ID, sd.StorageNode.ID, false, nil, order.ID); err != nil {
		t.Fatalf("arrival: %v", err)
	}
	var stillLifted bool
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT lifted_at IS NOT NULL FROM bins WHERE id=$1`, bin.ID).Scan(&stillLifted), "lifted_at after arrival")
	if stillLifted {
		t.Error("lifted_at survived the arrival")
	}
}

// TestLift_AStorageLiftEndsNoGeneration — D11 (i) scope: a bin lifted from a
// storage slot that no station streamed is not bumped (three statements for
// no reader, DESIGN-H §11(c)).
func TestLift_AStorageLiftEndsNoGeneration(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	manifest := announcingService(db)
	binSvc := NewBinService(db, manifest)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-STORLIFT", "PART-A", 60)
	e := binEpoch(t, db, bin.ID)
	testutil.MustNoErr(t, binSvc.MoveToTransit(bin.ID), "lift from storage")
	if got := binEpoch(t, db, bin.ID); got != e {
		t.Errorf("a storage lift moved delta_epoch %d -> %d", e, got)
	}
	if n := ledgerOps(t, db, bin.ID, "lifted"); n != 0 {
		t.Errorf("lifted rows = %d, want 0", n)
	}
}
