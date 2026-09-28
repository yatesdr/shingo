//go:build docker

package www

import (
	"net/http"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/audit"
)

// bin_load_door_test.go — the bin-load telemetry door answers like its
// bin-count and bin-clear siblings (I2).
//
// It took binList[0] on a node holding several carriers, loaded an unknown
// payload at uop 0 (a write that looks like a measurement of an empty bin),
// and ran the manifest write and the confirm as two transactions with the
// confirm's failure logged and swallowed — a set-but-unconfirmed bin reported
// "ok". The confirm is the gate that makes a full bin a drain source, so that
// was a bin the Edge thought it had loaded and kanban could never see.

func TestApiBinLoad_AmbiguousMultipleBins_Returns409(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	a := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LD-AMB-1")
	b := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LD-AMB-2")

	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", map[string]any{
		"node_name": sd.StorageNode.Name, "payload_code": sd.Payload.Code,
		"manifest": []map[string]any{{"part_number": "P1"}},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "specify bin_id")
	for _, id := range []int64{a.ID, b.ID} {
		if got, _ := db.GetBin(id); got.PayloadCode != "" {
			t.Errorf("bin %d loaded by a refused request: payload %q", id, got.PayloadCode)
		}
	}
}

func TestApiBinLoad_BinIDLoadsTheNamedBin(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	a := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LD-NAMED-1")
	b := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LD-NAMED-2")

	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", map[string]any{
		"node_name": sd.StorageNode.Name, "bin_id": b.ID, "payload_code": sd.Payload.Code,
		"uop_count": 12, "manifest": []map[string]any{{"part_number": "P1"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := db.GetBin(b.ID); got.PayloadCode != sd.Payload.Code || got.UOPRemaining != 12 || !got.ManifestConfirmed {
		t.Errorf("named bin: got payload %q uop %d confirmed %v", got.PayloadCode, got.UOPRemaining, got.ManifestConfirmed)
	}
	if got, _ := db.GetBin(a.ID); got.PayloadCode != "" {
		t.Errorf("the other bin was loaded: payload %q", got.PayloadCode)
	}

	// A bin that is not at the node is a 409, not a silent fall-back.
	rec = postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", map[string]any{
		"node_name": sd.StorageNode.Name, "bin_id": 987654321, "payload_code": sd.Payload.Code,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("foreign bin_id: status %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "is not at node")
}

// TestApiBinLoad_UnknownPayloadIsRefused: a code Core has no template for is
// refused like BinService.LoadPayload refuses it, declared count or not, and
// the bin is left as it was.
func TestApiBinLoad_UnknownPayloadIsRefused(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LD-UNKNOWN")

	for _, body := range []map[string]any{
		{"node_name": sd.StorageNode.Name, "payload_code": "PAYLOAD-CORE-HAS-NEVER-SEEN"},
		{"node_name": sd.StorageNode.Name, "payload_code": "PAYLOAD-CORE-HAS-NEVER-SEEN", "uop_count": 5},
	} {
		rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertJSONError(t, rec.Body.Bytes(), "not found")
	}
	if got, _ := db.GetBin(bin.ID); got.PayloadCode != bin.PayloadCode || got.UOPRemaining != bin.UOPRemaining {
		t.Errorf("refused load wrote the bin: payload %q uop %d, was %q uop %d",
			got.PayloadCode, got.UOPRemaining, bin.PayloadCode, bin.UOPRemaining)
	}
}

// TestApiBinLoad_WritesBothLedgerRowsOnce: the set and the confirm each leave
// their audit row, once, now from one transaction.
func TestApiBinLoad_WritesBothLedgerRowsOnce(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "BIN-LD-LEDGER")

	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", map[string]any{
		"node_name": sd.StorageNode.Name, "payload_code": sd.Payload.Code, "uop_count": 3,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	rows, err := audit.ListBinUOPByBin(db.DB, bin.ID, 100, 0)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	count := map[string]int{}
	for _, r := range rows {
		count[r.Op]++
	}
	if count[audit.OpSetForProduction] != 1 || count[audit.OpManifestConfirmed] != 1 {
		t.Errorf("ledger ops = %v, want exactly one %s and one %s", count,
			audit.OpSetForProduction, audit.OpManifestConfirmed)
	}
}
