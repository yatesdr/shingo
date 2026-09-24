//go:build docker

package www

import (
	"net/http"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// Characterisation pins for the two doors that write a counted number
// (SYNTH-round2 S7, V6): the bins-page cycle count and the Edge's
// count-from-the-line (apiBinCount). Both land in BinService.RecordCount.

// TestPin_AdminCount_BroadcastsCoresNumberUnfenced pins what the bins-page
// count tells the stations today: exactly one UOPAdjustment carrying the
// counted number and the bin's current epoch, and nothing that would let a
// station tell which of its own count messages Core had applied when it wrote
// that number. The station overwrites its count with it (the Edge half of P0f,
// shingo-edge engine TestPin_P0f_EdgeTakesCoresNumberOverItsInFlightTicks).
//
// VERIFY-RED: the record-count fence adds AsOfNet/AsOfSeq/AsOfStation, read
// inside the RecordCount transaction, and moves the announcement into that
// transaction (one row, not two).
func TestPin_AdminCount_BroadcastsCoresNumberUnfenced(t *testing.T) {
	t.Parallel()
	h, db, _, bin := setupBinForAction(t)

	testutil.MustNoErr(t, h.executeBinAction(bin, "record_count",
		mustJSON(t, map[string]any{"actual_uop": 95, "actor": "counter-1"})), "record_count")

	adjs := adjustmentsForBin(t, db, bin.ID)
	if len(adjs) != 1 {
		t.Fatalf("admin count enqueued %d UOPAdjustments, want 1", len(adjs))
	}
	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if adjs[0].NewRemaining != 95 || adjs[0].Epoch != got.DeltaEpoch {
		t.Errorf("adjustment = {remaining %d, epoch %d}, want {95, %d}",
			adjs[0].NewRemaining, adjs[0].Epoch, got.DeltaEpoch)
	}
}

// TestPin_LineCount_BroadcastsNothing pins flint's finding (V6 review): the
// count taken at the line reaches Core through apiBinCount and writes
// uop_remaining, and no UOPAdjustment goes out. The station that took the
// count writes the HTTP reply's number; any other station modelling the node
// keeps its old count. The handler's doc says it "broadcasts the same
// correction back down"; it does not.
//
// VERIFY-RED: with the fence, the line count broadcasts like the bins-page
// count, and the station that took it rebases instead of being overwritten.
func TestPin_LineCount_BroadcastsNothing(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-LINE-COUNT")

	rec := postJSON(t, h.apiBinCount, "/api/telemetry/bin-count", map[string]any{
		"node_name": sd.StorageNode.Name, "actual_uop": 7, "actor": "line-operator",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if adjs := adjustmentsForBin(t, db, bin.ID); len(adjs) != 0 {
		t.Errorf("line count enqueued %d UOPAdjustments, want 0 (today it broadcasts nothing)", len(adjs))
	}
}
