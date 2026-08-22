//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/audit"
)

// Item 10 audit-UI backend tests. Each endpoint round-trips a real
// bin_uop_ledger row through the JSON shape so a future column rename
// surfaces here, not at a dashboard.

// TestApiAuditBinTimeline pins the per-bin endpoint: every audit row
// for a given bin id, newest first.
func TestApiAuditBinTimeline(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-AUDIT-1")

	router := chi.NewRouter()
	router.Get("/api/audit/bin/{id}", h.apiAuditBinTimeline)

	for i := 0; i < 2; i++ {
		v := i * 5
		if err := audit.AppendBinUOP(db.DB, bin.ID, &v, v+1,
			audit.OpReleasedPartial, "test", nil, sd.Payload.Code, "OPERATOR-AB", audit.BinUOPContext{}); err != nil {
			t.Fatalf("seed audit %d: %v", i, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/audit/bin/%d", bin.ID), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var rows []audit.BinUOPRow
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&rows), "decode")
	if len(rows) < 2 {
		t.Errorf("rows = %d, want >= 2", len(rows))
	}
	for _, r := range rows {
		if r.BinID != bin.ID {
			t.Errorf("row BinID = %d, want %d", r.BinID, bin.ID)
		}
	}
}

// TestApiAuditBinTimeline_BadID pins the parse-error branch.
func TestApiAuditBinTimeline_BadID(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)
	router := chi.NewRouter()
	router.Get("/api/audit/bin/{id}", h.apiAuditBinTimeline)

	req := httptest.NewRequest(http.MethodGet, "/api/audit/bin/abc", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad-id status = %d, want 400", rec.Code)
	}
}

// TestApiAuditEnrichmentColumns pins the keystone step-2 analytics columns
// (node_id / station / loader_id) round-tripping through INSERT + SELECT + scan +
// JSON. loader_id is a PLAIN value (no FK) stamped at event time, so an arbitrary id
// is legitimate — that is exactly the property that lets it survive a loader archive.
func TestApiAuditEnrichmentColumns(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-ENRICH-1")

	router := chi.NewRouter()
	router.Get("/api/audit/bin/{id}", h.apiAuditBinTimeline)

	nodeID := sd.StorageNode.ID
	loaderID := int64(4242)
	v := 12
	if err := audit.AppendBinUOP(db.DB, bin.ID, &v, 6,
		audit.OpSetForProduction, "test", nil, sd.Payload.Code, "OP-ENRICH",
		audit.BinUOPContext{NodeID: &nodeID, LoaderID: &loaderID, Station: "ST-ENRICH"}); err != nil {
		t.Fatalf("seed enriched audit: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/audit/bin/%d", bin.ID), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var rows []audit.BinUOPRow
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&rows), "decode")
	if len(rows) == 0 {
		t.Fatal("no audit rows")
	}
	r := rows[0]
	if r.NodeID == nil || *r.NodeID != nodeID {
		t.Errorf("NodeID = %v, want %d", r.NodeID, nodeID)
	}
	if r.LoaderID == nil || *r.LoaderID != loaderID {
		t.Errorf("LoaderID = %v, want %d", r.LoaderID, loaderID)
	}
	if r.Station != "ST-ENRICH" {
		t.Errorf("Station = %q, want ST-ENRICH", r.Station)
	}
}
