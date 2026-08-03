//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

// TestNodeBins_DoesNotReportAnUnreadableNodeAsEmpty is the Core half of the
// fail-closed rule.
//
// The node-bins row is the sentence Edge's reservation seam reads to decide
// whether a loader window is free. It used to answer 200 with occupied=false
// when the database read FAILED, which is not the same fact and is the
// 2026-07-31 Springfield over-ordering incident seen from the other end of the
// wire: a hiccup here manufactures a free window, and an empty gets ordered onto
// an occupied one.
//
// Edge's own read now distinguishes "no answer" from "no bin", but it can only
// see the ways Edge can see — a transport failure, a non-200, an undecodable
// body. It cannot see through a 200 that says occupied=false. So Core must not
// send one it did not verify.
func TestNodeBins_DoesNotReportAnUnreadableNodeAsEmpty(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "BIN-TELEM-FAB")

	ask := func(t *testing.T) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/telemetry/node-bins?nodes="+sd.LineNode.Name, nil)
		rec := httptest.NewRecorder()
		h.apiTelemetryNodeBins(rec, req)
		return rec
	}

	// Baseline: with a working database the node reads occupied, so the failure
	// below is the read breaking and not the fixture being wrong.
	rec := ask(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy read: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var rows []struct {
		NodeName string `json:"node_name"`
		Occupied bool   `json:"occupied"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&rows); err != nil {
		t.Fatalf("decode healthy response: %v", err)
	}
	if len(rows) != 1 || !rows[0].Occupied {
		t.Fatalf("healthy read reported %+v, want one occupied row — the fixture bin is not being seen", rows)
	}

	// Now break the database underneath the handler.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	rec = ask(t)
	if rec.Code == http.StatusOK {
		t.Fatalf("a failed database read answered 200: %s\n"+
			"Edge cannot see through a 200, so this row is indistinguishable from a "+
			"verified empty window and an empty will be ordered onto an occupied one.",
			rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d — Edge classifies a non-200 as unreachable and holds", rec.Code, http.StatusInternalServerError)
	}
}
