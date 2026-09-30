//go:build docker

package uop_test

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/uop"
)

// The tail of a produce bin (S1b, R4-5): the ticks of the Edge's last flush
// window, counted under the bin's epoch, and the release-time ingest, which
// bumps it. The Edge flushes before it queues the ingest, so Core applies the
// window first and drops nothing. The reverse order — the ingest first, the
// window after — drops the window as stale; that is the control, and what
// the release path did with no flush.

func staleEpochRows(t *testing.T, db *store.DB, binID int64) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM bin_uop_exception WHERE bin_id=$1 AND kind='stale_epoch'`,
		binID).Scan(&n), "stale_epoch rows")
	return n
}

func TestS1bTail_FlushBeforeIngestDropsNothing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	mani := service.NewBinManifestService(db, service.EpochAnnounce{})
	svc := uop.NewInventoryDeltaService(db, mani, service.EpochAnnounce{})
	payloadWithTwoLines(t, db, "SYNTAIL")

	run := func(label string, ingestFirst bool) int {
		bin := createTestBin(t, db, sd.StorageNode.ID, label, "", 0)
		// The bin is filling: its first ticks bind the payload.
		first := makeBinDelta(bin.ID, "SYNTAIL", 42, 1, protocol.ReasonProduceTick)
		first.Epoch = bin.DeltaEpoch
		testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, first), "first window")
		b, err := db.GetBin(bin.ID)
		testutil.MustNoErr(t, err, "read bin")
		tail := makeBinDelta(bin.ID, "SYNTAIL", 5, 2, protocol.ReasonProduceTick)
		tail.Epoch = b.DeltaEpoch
		qty := 47
		ingest := func() {
			testutil.MustNoErr(t, mani.RecordProducedBinFromTemplate(bin.ID, "SYNTAIL", &qty, ""), "ingest")
		}
		if ingestFirst {
			ingest()
			if err := svc.ApplyBinUOPDelta(testStation, tail); err == nil {
				t.Error("control: the tail after the ingest applied; want it dropped as stale")
			}
		} else {
			testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, tail), "tail before ingest")
			ingest()
		}
		return staleEpochRows(t, db, bin.ID)
	}

	if n := run("SYN-BIN-TAIL-FLUSHED", false); n != 0 {
		t.Errorf("flush then ingest: stale_epoch rows = %d, want 0", n)
	}
	if n := run("SYN-BIN-TAIL-CONTROL", true); n != 1 {
		t.Errorf("control, ingest then tail: stale_epoch rows = %d, want 1 (the window dropped)", n)
	}
}
