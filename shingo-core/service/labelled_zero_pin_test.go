//go:build docker

package service

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// TestPin_ReleaseAtOneThenATickLeavesTheCarrierLabelledAtZero characterises the
// Core ledger half of the SHIM loop (RULINGS-C2b-rebase6-and-loader-orphan §3,
// demo r6 ledger 4328-4329). The final release states 1; Core writes 1 and
// starts a new generation; one more tick, stamped with the new generation,
// lands after the lift; the carrier arrives back at a loader still labelled,
// reading 0. The pool then refuses it both ways (dispatch/binsource
// TestPin_LabelledCarrierAtZeroIsNeitherStockNorContainer) and the loader
// refuses to load it (shingo-edge TestPin_LoaderRefusesALabelledCarrierAtZero).
//
// C2b does not change this. C2c Lane H's mechanism (i) may move where the
// generation ends; that change converts this pin, it does not retire it.
func TestPin_ReleaseAtOneThenATickLeavesTheCarrierLabelledAtZero(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	manifest := announcingService(db)
	binSvc := NewBinService(db, manifest)
	deltas := NewInventoryDeltaService(db, manifest, EpochAnnounce{Topic: announceTopic, CoreStation: "core.test"})

	bin := createTestBin(t, db, sd.LineNode.ID, "BIN-LZ", "PART-A", 1)
	order := createTestOrder(t, db, sd.StorageNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)
	before := binEpoch(t, db, bin.ID)

	one := 1
	testutil.MustNoErr(t, manifest.SyncOrClearForReleased(bin.ID, order.ID, &one,
		protocol.DispositionReleasePartial, "sim-operator"), "release partial 1")
	after := binEpoch(t, db, bin.ID)
	if after != before+1 {
		t.Fatalf("release partial left delta_epoch %d -> %d, want a new generation", before, after)
	}

	testutil.MustNoErr(t, binSvc.MoveToTransit(bin.ID), "MoveToTransit (the lift)")
	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(pinStation, consumeDelta(bin.ID, after, 1, -1)),
		"the tick after the release, stamped with the new generation")
	if _, err := binSvc.ApplyArrival(bin.ID, sd.StorageNode.ID, false, nil, order.ID); err != nil {
		t.Fatalf("ApplyArrival at the loader: %v", err)
	}

	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.UOPRemaining != 0 {
		t.Errorf("uop_remaining = %d, want 0 (the tick applied in transit)", got.UOPRemaining)
	}
	if got.PayloadCode != "PART-A" {
		t.Errorf("payload_code = %q, want PART-A: a consume tick does not clear a label", got.PayloadCode)
	}
}
