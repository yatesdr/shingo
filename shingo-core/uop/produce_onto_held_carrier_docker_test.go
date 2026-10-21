//go:build docker

package uop_test

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store/orders"
	"shingocore/uop"
)

// The two produce writers record what a press already put into a carrier, and
// that carrier is routinely held by then: the press's swap or removal order
// claimed it at dispatch. Neither is a LOAD, and the held-carrier check the
// LOAD doors run must not reach them (owner, 2026-10-18: "the two produce
// writers" stay legal and identical). Pinned here against a carrier a
// dispatched order claims, the hold a LOAD is refused under
// (www.TestApiBinLoad_AHeldCarrierIsRefused).

func TestProduceWriters_RecordOntoAHeldCarrier(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	mani := service.NewBinManifestService(db, service.EpochAnnounce{})
	svc := uop.NewInventoryDeltaService(db, mani, service.EpochAnnounce{})
	payloadWithTwoLines(t, db, "HELDPROD")

	held := func(label string) int64 {
		bin := createTestBin(t, db, sd.StorageNode.ID, label, "", 0)
		o := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusDispatched })
		testdb.ClaimBinForTest(t, db, bin.ID, o.ID)
		return bin.ID
	}

	// The produce tick's bind: the first tick on a blank carrier stamps the part.
	tick := held("BIN-HELD-TICK")
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation,
		makeBinDelta(tick, "HELDPROD", 3, 1, protocol.ReasonProduceTick)), "a produce tick on a held carrier")
	assertManifestMatchesLoadPath(t, db, mani, sd.StorageNode.ID, tick, "HELDPROD")
	if got := testdb.RequireBin(t, db, tick); got.UOPRemaining != 3 {
		t.Errorf("produce tick: uop %d, want 3", got.UOPRemaining)
	}

	// Produce finalize: the press's count and the confirm, in one write.
	fin := held("BIN-HELD-FINAL")
	testutil.MustNoErr(t, mani.RecordProducedBin(fin, `{"items":[{"part_number":"HELDPROD-PN-Z"}]}`, "HELDPROD", 40,
		"2026-10-18T06:00:00Z", nil), "produce finalize on a held carrier")
	got := testdb.RequireBin(t, db, fin)
	if got.PayloadCode != "HELDPROD" || got.UOPRemaining != 40 || !got.ManifestConfirmed {
		t.Errorf("produce finalize: payload %q uop %d confirmed %v, want HELDPROD 40 true",
			got.PayloadCode, got.UOPRemaining, got.ManifestConfirmed)
	}
}
