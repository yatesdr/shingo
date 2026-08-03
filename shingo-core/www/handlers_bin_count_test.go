//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// The count-from-the-line door. Core could declare a count downward — a clear,
// a load, or a cycle count on the bins page — but the only way a number could
// come UP was riding an order release. An operator standing at the line who saw
// that a carrier's count was wrong had nowhere to say so, and that is the
// number most likely to be right.

func TestApiBinCount_RecordsTheDeclaredCount(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-COUNT-1")

	before, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin before")

	rec := postJSON(t, h.apiBinCount, "/api/telemetry/bin-count", map[string]any{
		"node_name": sd.StorageNode.Name, "actual_uop": 7, "actor": "line-operator",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status       string `json:"status"`
		BinID        int64  `json:"bin_id"`
		Expected     int    `json:"expected"`
		UOPRemaining int    `json:"uop_remaining"`
		DeltaEpoch   int64  `json:"delta_epoch"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if resp.Status != "ok" || resp.BinID != bin.ID || resp.UOPRemaining != 7 {
		t.Errorf("response: got %+v, want the carrier and the declared 7", resp)
	}
	if resp.Expected != before.UOPRemaining {
		t.Errorf("expected = %d, want %d — the reply has to say what it was, or the "+
			"operator cannot see the size of the correction", resp.Expected, before.UOPRemaining)
	}

	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin after")
	if got.UOPRemaining != 7 {
		t.Errorf("uop_remaining = %d, want 7 — the ledger has to take the number from the "+
			"line, which is the side standing next to the carrier", got.UOPRemaining)
	}
}

// TestApiBinCount_DoesNotStartANewGeneration is the one thing this door must
// NOT do. A count correction fixes a number inside the carrier's current life;
// a new generation is for a carrier that has been emptied and reloaded. Bumping
// here would retire a life that is still running, and the station's very next
// report would arrive stamped with a generation Core had just ended and be
// thrown away — the exact failure the rest of this work removes, reintroduced
// by the fix for it.
func TestApiBinCount_DoesNotStartANewGeneration(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-COUNT-2")

	before, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin before")

	rec := postJSON(t, h.apiBinCount, "/api/telemetry/bin-count", map[string]any{
		"node_name": sd.StorageNode.Name, "actual_uop": 12,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin after")
	if got.DeltaEpoch != before.DeltaEpoch {
		t.Errorf("delta_epoch went %d → %d — a count correction is not a new life for the "+
			"carrier, and retiring the running one makes the station's next report stale",
			before.DeltaEpoch, got.DeltaEpoch)
	}
}

// TestApiBinCount_ClearsTheGoCountThisFlag: the flag means "somebody go count
// this". Counting it has to clear it, or it ratchets — which it did, at both
// plants, until that was fixed on the bins-page path. The door from the line
// goes through the same service, so it inherits the same behaviour, and this
// test is what says so out loud.
func TestApiBinCount_ClearsTheGoCountThisFlag(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-COUNT-3")

	_, err := db.Exec(`UPDATE bins SET anomaly_at=NOW() WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "flag the carrier")

	rec := postJSON(t, h.apiBinCount, "/api/telemetry/bin-count", map[string]any{
		"node_name": sd.StorageNode.Name, "actual_uop": 3,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var flagged *string
	testutil.MustNoErr(t, db.QueryRow(`SELECT anomaly_at::text FROM bins WHERE id=$1`, bin.ID).Scan(&flagged), "read flag")
	if flagged != nil {
		t.Errorf("anomaly_at = %q, want cleared — a flag that survives being answered is a "+
			"ratchet, and the board can never be returned to clean", *flagged)
	}
}

func TestApiBinCount_RefusesAnEmptyNode(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	rec := postJSON(t, h.apiBinCount, "/api/telemetry/bin-count", map[string]any{
		"node_name": sd.StorageNode.Name, "actual_uop": 5,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 for a node holding nothing; body=%s", rec.Code, rec.Body.String())
	}
}

func TestApiBinCount_RefusesANegativeCount(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-COUNT-4")

	rec := postJSON(t, h.apiBinCount, "/api/telemetry/bin-count", map[string]any{
		"node_name": sd.StorageNode.Name, "actual_uop": -1,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
