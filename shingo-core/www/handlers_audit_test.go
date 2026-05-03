//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingocore/internal/testdb"
	"shingocore/store/audit"
)

// Item 10 audit-UI backend tests. Each endpoint round-trips a real
// bin_uop_audit row through the JSON shape so a future column rename
// surfaces here, not at a dashboard.

func newAuditRouter(t *testing.T) (*Handlers, *chi.Mux) {
	t.Helper()
	h, _ := testHandlers(t)
	r := chi.NewRouter()
	r.Get("/api/audit/bin/{id}", h.apiAuditBinTimeline)
	r.Get("/api/audit/operator/{name}", h.apiAuditOperatorActivity)
	r.Get("/api/audit/station/{station}", h.apiAuditStationOverrides)
	return h, r
}

// TestApiAuditBinTimeline pins the per-bin endpoint: every audit row
// for a given bin id, newest first.
func TestApiAuditBinTimeline(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-AUDIT-1")

	router := chi.NewRouter()
	router.Get("/api/audit/bin/{id}", h.apiAuditBinTimeline)

	for i := 0; i < 2; i++ {
		v := i * 5
		if err := audit.AppendBinUOP(db.DB, bin.ID, &v, v+1,
			audit.OpReleasedPartial, "test", nil, sd.Payload.Code, "OPERATOR-AB"); err != nil {
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
	if err := json.NewDecoder(rec.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
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

// TestApiAuditOperatorActivity pins the by-actor endpoint: rows
// where actor matches the URL param exactly.
func TestApiAuditOperatorActivity(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-OPER-1")

	router := chi.NewRouter()
	router.Get("/api/audit/operator/{name}", h.apiAuditOperatorActivity)

	v := 10
	if err := audit.AppendBinUOP(db.DB, bin.ID, &v, 8,
		audit.OpReleasedPartial, "test", nil, "", "OP-OPER-1"); err != nil {
		t.Fatalf("seed audit: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/audit/operator/OP-OPER-1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var rows []audit.BinUOPRow
	if err := json.NewDecoder(rec.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) == 0 {
		t.Errorf("got 0 rows, expected at least one for OP-OPER-1")
	}
	for _, r := range rows {
		if r.Actor != "OP-OPER-1" {
			t.Errorf("Actor = %q, want OP-OPER-1", r.Actor)
		}
	}
}

// TestApiAuditStationOverrides pins the station-override filter:
// only the two operator-override op tags surface, no other audit rows.
func TestApiAuditStationOverrides(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-STN-1")

	router := chi.NewRouter()
	router.Get("/api/audit/station/{station}", h.apiAuditStationOverrides)

	// Mix: one override row + one regular release row, both for the
	// same actor (station id). Only the override should surface.
	v1 := 12
	if err := audit.AppendBinUOPOverride(db.DB, bin.ID, v1, 11,
		audit.OpOperatorOverridePullParts, "test", nil, sd.Payload.Code, "STATION-X",
		[]byte(`{"kind":"pull_parts"}`)); err != nil {
		t.Fatalf("seed override: %v", err)
	}
	v2 := 7
	if err := audit.AppendBinUOP(db.DB, bin.ID, &v2, 6,
		audit.OpReleasedPartial, "test", nil, sd.Payload.Code, "STATION-X"); err != nil {
		t.Fatalf("seed regular: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/audit/station/STATION-X", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var rows []audit.BinUOPRow
	if err := json.NewDecoder(rec.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (override only): %+v", len(rows), rows)
	}
	if rows[0].Op != audit.OpOperatorOverridePullParts {
		t.Errorf("Op = %q, want %q", rows[0].Op, audit.OpOperatorOverridePullParts)
	}
}
