//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/loaders"
)

// loader_edge_pins_docker_test.go — a station's Edge through Core's loader
// doors: the update stores the uid and the list hands it back with that Edge's
// display name beside the loader; a uid Core has no Edge for is a 400 that
// names it; a list whose registry read fails still lists, with no names. The
// page's half (the pick, its words, the full-row write) is
// testdata/static/pages/loaders.flow.test.js.

// listedEdge is the loader's stored Edge uid and the display name the list
// gives it.
func listedEdge(t *testing.T, h *Handlers, id int64) (uid, name string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.apiListLoaders(rec, httptest.NewRequest(http.MethodGet, "/api/loader/list", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Loaders []struct {
			Loader struct {
				ID             int64  `json:"id"`
				EdgeStationUID string `json:"station_uid"`
			} `json:"loader"`
			EdgeDisplayName string `json:"edge_display_name"`
		} `json:"loaders"`
	}
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &body), "decode the list")
	for _, l := range body.Loaders {
		if l.Loader.ID == id {
			return l.Loader.EdgeStationUID, l.EdgeDisplayName
		}
	}
	t.Fatalf("loader %d not listed", id)
	return "", ""
}

// TestLoaderUpdate_EdgeStationUIDRoundTrip: the update door stores an enrolled
// Edge's uid and the list names it; an unknown uid is refused 400 with the uid
// in the words and nothing lands; a save without the field picks no Edge.
func TestLoaderUpdate_EdgeStationUIDRoundTrip(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	_, err := db.EnrollEdge("stn-lep-press", "PRESS EDGE")
	testutil.MustNoErr(t, err, "enroll the Edge")
	id, err := db.CreateLoader(loaders.Loader{Name: "LEP-LDR", Role: loaders.RoleProduce,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator})
	testutil.MustNoErr(t, err, "a loader")
	if uid, name := listedEdge(t, h, id); uid != "" || name != "" {
		t.Fatalf("a new loader lists Edge %q (%q), want none", uid, name)
	}

	rec := postJSON(t, h.apiUpdateLoader, "/api/loader/update",
		map[string]any{"id": id, "name": "LEP-LDR", "station_uid": "stn-lep-press"})
	if rec.Code != http.StatusOK {
		t.Fatalf("pick the Edge: %d %s", rec.Code, rec.Body.String())
	}
	if uid, name := listedEdge(t, h, id); uid != "stn-lep-press" || name != "PRESS EDGE" {
		t.Fatalf("picked: listed %q (%q), want stn-lep-press (PRESS EDGE)", uid, name)
	}

	rec = postJSON(t, h.apiUpdateLoader, "/api/loader/update",
		map[string]any{"id": id, "name": "LEP-LDR", "station_uid": "stn-lep-nobody"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `no Edge with station uid \"stn-lep-nobody\"`) {
		t.Errorf("an unknown uid: %d %s; want 400 naming the uid", rec.Code, rec.Body.String())
	}
	if uid, _ := listedEdge(t, h, id); uid != "stn-lep-press" {
		t.Errorf("after the refusal: listed %q, want the pick before it", uid)
	}

	rec = postJSON(t, h.apiUpdateLoader, "/api/loader/update", map[string]any{"id": id, "name": "LEP-LDR"})
	if rec.Code != http.StatusOK {
		t.Fatalf("a save without the field: %d %s", rec.Code, rec.Body.String())
	}
	if uid, name := listedEdge(t, h, id); uid != "" || name != "" {
		t.Errorf("a save without the field: listed %q (%q), want no Edge", uid, name)
	}
}

// TestListLoaders_EdgeNamesDegrade: a list whose registry read fails still
// lists every loader, with its uid and no names, rather than failing the page.
func TestListLoaders_EdgeNamesDegrade(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	_, err := db.EnrollEdge("stn-lep-cell", "CELL EDGE")
	testutil.MustNoErr(t, err, "enroll the Edge")
	id, err := db.CreateLoader(loaders.Loader{Name: "LEP-CELL", Role: loaders.RoleProduce,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator, StationUID: "stn-lep-cell"})
	testutil.MustNoErr(t, err, "a loader with an Edge")
	_, err = db.Exec(`ALTER TABLE edge_registry RENAME TO edge_registry_hidden`)
	testutil.MustNoErr(t, err, "hide the registry")
	if uid, name := listedEdge(t, h, id); uid != "stn-lep-cell" || name != "" {
		t.Errorf("registry unread: listed %q (%q), want the uid and no name", uid, name)
	}
}
