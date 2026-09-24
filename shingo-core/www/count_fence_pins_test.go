//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// The two doors that write a counted number (SYNTH-round2 S7, V6): the
// bins-page cycle count and the Edge's count-from-the-line (apiBinCount).
// Both land in BinService.RecordCount, which enqueues the one UOPAdjustment
// inside its transaction, fenced with the counting station's cursor.

// seedCursor gives Core a dedup row for (station, bin, epoch), as if the
// station had sent count messages up to seq with running net net.
func seedCursor(t *testing.T, db *store.DB, station string, binID, epoch, seq, net int64) {
	t.Helper()
	_, err := db.DB.Exec(`INSERT INTO inventory_delta_dedup
		(station, scope_kind, scope_key, epoch, last_seq, applied_net) VALUES ($1, 'bin', $2, $3, $4, $5)`,
		station, strconv.FormatInt(binID, 10), epoch, seq, net)
	testutil.MustNoErr(t, err, "seed cursor")
}

// TestAdminCount_BroadcastsCoresNumberFenced is the inverted
// TestPin_AdminCount_BroadcastsCoresNumberUnfenced. The bins-page count used to
// enqueue its broadcast after the transaction, with nothing to say which of the
// station's count messages Core had applied; the station overwrote its own
// number with it. It now carries the station's cursor, and there is still
// exactly one message (the door's own after-commit send is gone).
func TestAdminCount_BroadcastsCoresNumberFenced(t *testing.T) {
	t.Parallel()
	h, db, _, bin := setupBinForAction(t)
	seedCursor(t, db, "stn-www", bin.ID, bin.DeltaEpoch, 4, -12)

	testutil.MustNoErr(t, h.executeBinAction(bin, "record_count",
		mustJSON(t, map[string]any{"actual_uop": 95, "actor": "counter-1"})), "record_count")

	adjs := adjustmentsForBin(t, db, bin.ID)
	if len(adjs) != 1 {
		t.Fatalf("admin count enqueued %d UOPAdjustments, want 1", len(adjs))
	}
	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	a := adjs[0]
	if a.NewRemaining != 95 || a.Epoch != got.DeltaEpoch {
		t.Errorf("adjustment = {remaining %d, epoch %d}, want {95, %d}", a.NewRemaining, a.Epoch, got.DeltaEpoch)
	}
	if a.AsOfNet == nil || *a.AsOfNet != -12 || a.AsOfSeq == nil || *a.AsOfSeq != 4 || a.AsOfStation != "stn-www" {
		t.Errorf("fence = {%v, %v, %q}, want {-12, 4, stn-www}", a.AsOfNet, a.AsOfSeq, a.AsOfStation)
	}
	if a.Actor != "counter-1" {
		t.Errorf("actor = %q, want the counter", a.Actor)
	}
}

// TestLineCount_BroadcastsFencedAndRepliesWithTheFence is the inverted
// TestPin_LineCount_BroadcastsNothing (flint's finding). The count from the
// line now broadcasts like the bins-page count, so a second station modelling
// the node hears it, and the reply carries the same fence so the station that
// took the count rebases on it rather than writing Core's number over the
// ticks it consumed while the request was out.
func TestLineCount_BroadcastsFencedAndRepliesWithTheFence(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-LINE-COUNT")
	seedCursor(t, db, "stn-line", bin.ID, bin.DeltaEpoch, 9, -30)

	rec := postJSON(t, h.apiBinCount, "/api/telemetry/bin-count", map[string]any{
		"node_name": sd.StorageNode.Name, "actual_uop": 7, "actor": "line-operator",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		UOPRemaining int    `json:"uop_remaining"`
		DeltaEpoch   int64  `json:"delta_epoch"`
		AsOfNet      *int64 `json:"as_of_net"`
		AsOfSeq      *int64 `json:"as_of_seq"`
		AsOfStation  string `json:"as_of_station"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode reply")
	if resp.AsOfNet == nil || *resp.AsOfNet != -30 || resp.AsOfSeq == nil || *resp.AsOfSeq != 9 ||
		resp.AsOfStation != "stn-line" || resp.DeltaEpoch != bin.DeltaEpoch {
		t.Errorf("reply fence = {%v, %v, %q, epoch %d}, want {-30, 9, stn-line, %d}",
			resp.AsOfNet, resp.AsOfSeq, resp.AsOfStation, resp.DeltaEpoch, bin.DeltaEpoch)
	}

	adjs := adjustmentsForBin(t, db, bin.ID)
	if len(adjs) != 1 {
		t.Fatalf("line count enqueued %d UOPAdjustments, want 1", len(adjs))
	}
	if a := adjs[0]; a.NewRemaining != 7 || a.AsOfNet == nil || *a.AsOfNet != -30 || a.AsOfStation != "stn-line" {
		t.Errorf("adjustment = %+v, want remaining 7 fenced at -30 for stn-line", a)
	}
}
