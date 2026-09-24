//go:build docker

package service

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// Characterisation pins for the two Core-side writers that set uop_remaining
// to an absolute number (SYNTH-round2 V6, V7): RecordCount and
// syncUOPAndClaimTx. Each pin names the change that inverts it.

const pinStation = "stn-test"

func binEpoch(t *testing.T, db *store.DB, binID int64) int64 {
	t.Helper()
	var e int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, binID).Scan(&e),
		"read delta_epoch")
	return e
}

func binRemaining(t *testing.T, db *store.DB, binID int64) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, binID).Scan(&n),
		"read uop_remaining")
	return n
}

func consumeDelta(binID, epoch, seq int64, delta int) *protocol.BinUOPDelta {
	now := time.Now().UTC()
	return &protocol.BinUOPDelta{
		BinID:       binID,
		PayloadCode: "PART-A",
		Delta:       delta,
		Reason:      protocol.ReasonConsumeTick,
		SequenceID:  seq,
		Epoch:       epoch,
		WindowStart: now.Add(-5 * time.Second),
		WindowEnd:   now,
	}
}

// TestRecordCount_SameEpochDeltaAfterTheCountAppliesOnTop pins P0f, the Core
// half of the in-flight-delta race.
//
// The Edge has a -5 delta in flight when an operator counts N at Core.
// RecordCount writes N absolutely and does NOT bump delta_epoch, so the delta
// (stamped with the same epoch) is not stale when it arrives and applies on
// top: Core ends at N-5. The Edge, which adopted N from the count, ends at N.
// Both sides believe their number.
//
// VERIFY-RED: phase 2 of lane C (SYNTH-round2 S7, the RecordCount fence:
// UOPAdjustment.AsOfNet/AsOfSeq read in the RecordCount transaction and the
// Edge rebasing onto them) is the change that inverts this pin. It rewrites
// RecordCount; this pin's expectation is re-derived there.
func TestRecordCount_SameEpochDeltaAfterTheCountAppliesOnTop(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	manifest := announcingService(db)
	bins := NewBinService(db, manifest)
	deltas := NewInventoryDeltaService(db, manifest, EpochAnnounce{Topic: announceTopic, CoreStation: "core.test"})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-P0F", "PART-A", 100)
	epoch := binEpoch(t, db, bin.ID)

	// The Edge's first window lands normally: 100 -> 90.
	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(pinStation, consumeDelta(bin.ID, epoch, 1, -10)), "apply seq 1")

	// The operator counts 50 at Core while seq 2 (-5) is still in flight.
	fresh, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if _, err := bins.RecordCount(fresh, 50, "operator-under-test"); err != nil {
		t.Fatalf("RecordCount: %v", err)
	}
	if got := binRemaining(t, db, bin.ID); got != 50 {
		t.Fatalf("after RecordCount uop_remaining = %d, want 50 (an absolute write)", got)
	}
	if got := binEpoch(t, db, bin.ID); got != epoch {
		t.Fatalf("RecordCount moved delta_epoch %d -> %d; the pin is that it does not bump", epoch, got)
	}
	if adj := outboxAdjustments(t, db, bin.ID); len(adj) != 0 {
		t.Errorf("RecordCount enqueued %d UOPAdjustment rows; the service path announces nothing", len(adj))
	}

	// The in-flight delta arrives after the count, same epoch.
	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(pinStation, consumeDelta(bin.ID, epoch, 2, -5)), "apply seq 2")
	if got := binRemaining(t, db, bin.ID); got != 45 {
		t.Errorf("after the in-flight delta uop_remaining = %d, want 45 (N-5): the delta is "+
			"same-epoch, so it applies on top of the count", got)
	}
}

// TestSyncUOPAndClaim_WritesAbsoluteWithNoBumpAndNoAnnounce pins V7.
//
// The partial-consumption claim (dispatch/store_slot.go ConfirmClaim with
// order.RemainingUOP > 0 -> claimAndConfirm -> claimUnderReservationTx ->
// syncUOPAndClaimTx) writes uop_remaining absolutely, does not bump
// delta_epoch and enqueues no UOPAdjustment. A delta stamped with the epoch
// the station held before the sync is therefore still current, and it applies
// on top of the synced number.
//
// VERIFY-RED: SYNTH-round2 S7 ("syncUOPAndClaimTx goes through bumpEpoch")
// inverts all three halves: the epoch moves by one, one UOPAdjustment carrying
// the synced count and the new epoch is enqueued in the same transaction, and
// the old-epoch delta is stale-dropped (the count stays at the synced number).
func TestSyncUOPAndClaim_WritesAbsoluteWithNoBumpAndNoAnnounce(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	manifest := announcingService(db)
	deltas := NewInventoryDeltaService(db, manifest, EpochAnnounce{Topic: announceTopic, CoreStation: "core.test"})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SYNC", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	epoch := binEpoch(t, db, bin.ID)

	remaining := 37
	testutil.MustNoErr(t, manifest.ClaimForDispatch(bin.ID, order.ID, &remaining), "ClaimForDispatch(37)")

	if got := binRemaining(t, db, bin.ID); got != 37 {
		t.Fatalf("after the sync uop_remaining = %d, want 37 (an absolute write)", got)
	}
	if got := binEpoch(t, db, bin.ID); got != epoch {
		t.Errorf("syncUOPAndClaimTx moved delta_epoch %d -> %d; the pin is that it does not bump", epoch, got)
	}
	if adj := outboxAdjustments(t, db, bin.ID); len(adj) != 0 {
		t.Errorf("syncUOPAndClaimTx enqueued %d UOPAdjustment rows; the pin is that it announces nothing", len(adj))
	}

	// A late delta from the station, stamped with the epoch it held before.
	err := deltas.ApplyBinUOPDelta(pinStation, consumeDelta(bin.ID, epoch, 1, -2))
	if err != nil {
		t.Fatalf("old-epoch delta after the sync: %v, want nil (applied)", err)
	}
	if got := binRemaining(t, db, bin.ID); got != 35 {
		t.Errorf("after the old-epoch delta uop_remaining = %d, want 35: with no bump the "+
			"delta is still current and applies on top of the synced 37", got)
	}
}
