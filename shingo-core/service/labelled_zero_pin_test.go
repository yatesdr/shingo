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
// demo r6 ledger 4328-4329). The final release states 1; one more tick lands;
// the carrier arrives back at a loader still labelled, reading 0. The pool then
// refuses it both ways (dispatch/binsource
// TestPin_LabelledCarrierAtZeroIsNeitherStockNorContainer) and the loader
// refuses to load it (shingo-edge TestPin_LoaderRefusesALabelledCarrierAtZero).
//
// C2b did not change this. CONVERTED (Lane H, DECISIONS D11 (i), 2026-10-10),
// not retired: the generation no longer ends at the release, it ends at the
// lift. The release at 1 carries the stream fence and leaves the generation
// where it was; the tick, counted before the lift, is stamped with that same
// generation and lands (R1); the lift starts the next generation. The state
// the loader reads is unchanged — labelled, at 0 (R5). The bump expectation
// moved from the release to the lift.
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
	testutil.MustNoErr(t, manifest.SyncOrClearForReleasedFenced(bin.ID, order.ID, &one,
		protocol.DispositionReleasePartial, "sim-operator",
		&ReleaseFence{Station: pinStation, ReleaseFence: protocol.ReleaseFence{BinID: bin.ID, Epoch: before}}),
		"release partial 1")
	if after := binEpoch(t, db, bin.ID); after != before {
		t.Fatalf("release partial moved delta_epoch %d -> %d; a fenced release declares the count inside the generation", before, after)
	}

	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(pinStation, consumeDelta(bin.ID, before, 1, -1)),
		"the tick after the release, before the lift, stamped with the generation it still is")
	testutil.MustNoErr(t, binSvc.MoveToTransit(bin.ID), "MoveToTransit (the lift)")
	if lifted := binEpoch(t, db, bin.ID); lifted != before+1 {
		t.Errorf("the lift left delta_epoch %d -> %d, want a new generation: the lift ends it (D11 (i))", before, lifted)
	}
	if _, err := binSvc.ApplyArrival(bin.ID, sd.StorageNode.ID, false, nil, order.ID); err != nil {
		t.Fatalf("ApplyArrival at the loader: %v", err)
	}

	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.UOPRemaining != 0 {
		t.Errorf("uop_remaining = %d, want 0 (the tick before the lift is the carrier's)", got.UOPRemaining)
	}
	if got.PayloadCode != "PART-A" {
		t.Errorf("payload_code = %q, want PART-A: a consume tick does not clear a label", got.PayloadCode)
	}
}
