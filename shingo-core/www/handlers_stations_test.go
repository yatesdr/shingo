//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingocore/store/registry"
)

// GET /api/stations carries both halves of the identity/label split, and which
// half a caller uses decides whether this feature is safe.
//
// Every consumer is a picker or a filter that SUBMITS something:
// dashboards.stations_json stores the selected ids, cell_config.station stores
// the typed one, and the missions/overview filters put theirs in a query string
// matched against orders.station_id exactly. If a caller ever submits `label`,
// a mutable display name lands in a stored column and the board it scopes
// matches nothing — silently, because no row errors on a station that does not
// exist.

func TestAPIStations_ReturnsIDAndLabelSeparately(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	const uid = "plant-a.line-1"
	const label = "SPRINGFIELD / LINE 1"
	if _, err := registry.Enroll(db.DB, uid, label, uid); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	rec := httptest.NewRecorder()
	h.apiStations(rec, httptest.NewRequest(http.MethodGet, "/api/stations", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var got []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body = %s", err, rec.Body.String())
	}

	var found bool
	for _, s := range got {
		if s.ID != uid {
			continue
		}
		found = true
		if s.Label != label {
			t.Errorf("label = %q, want %q", s.Label, label)
		}
		// THE POINT: they are different strings, in different fields. A response
		// that collapsed them would still "work" for a picker and would put the
		// label in stations_json the first time somebody saved a dashboard.
		if s.ID == s.Label {
			t.Errorf("id and label are the same string (%q) — the enrolled display "+
				"name is not being returned separately from the identity", s.ID)
		}
	}
	if !found {
		t.Fatalf("enrolled station %q missing from /api/stations; got %+v", uid, got)
	}
}

// TestAPIStations_UnenrolledStationsLabelAsThemselves covers the values that
// have no registry row and never will — Core's synthetic order sources. A
// picker must still be able to offer them, and it must not show a blank.
func TestAPIStations_UnenrolledStationsLabelAsThemselves(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := httptest.NewRecorder()
	h.apiStations(rec, httptest.NewRequest(http.MethodGet, "/api/stations", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, s := range got {
		if s.Label == "" {
			t.Errorf("station %q came back with an empty label — an unenrolled "+
				"station must label as itself, not as blank", s.ID)
		}
	}
}
