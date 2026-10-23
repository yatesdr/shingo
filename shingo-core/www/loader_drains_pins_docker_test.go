//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// loader_drains_pins_docker_test.go — an unloader's four drain boxes through
// Core's loader doors: the update stores the set and the list returns it, so
// the page draws what it saved; a save carrying the set back with another
// setting keeps it; a box where it cannot hold is a 400 with the reason. The
// page's half (the boxes, their words, the full-row write) is
// static/pages/loaders.flow.test.js.

func listedDrains(t *testing.T, h *Handlers, id int64) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.apiListLoaders(rec, httptest.NewRequest(http.MethodGet, "/api/loader/list", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Loaders []struct {
			Loader struct {
				ID          int64    `json:"id"`
				DrainStates []string `json:"drain_states"`
			} `json:"loader"`
		} `json:"loaders"`
	}
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &body), "decode the list")
	for _, l := range body.Loaders {
		if l.Loader.ID == id {
			return l.Loader.DrainStates
		}
	}
	t.Fatalf("loader %d not listed", id)
	return nil
}

// TestLoaderUpdate_DrainBoxesRoundTrip: the update door stores the boxes and
// the list hands back the same set; saving another setting with the set sent
// back keeps it; a box on a fed-directly save is refused 400 and nothing lands.
func TestLoaderUpdate_DrainBoxesRoundTrip(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	src := &nodes.Node{Name: "DRW-PRESS-OUT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "the inbound source")
	id, err := db.CreateLoader(loaders.Loader{Name: "DRW-UNL", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator, InboundSource: src.Name})
	testutil.MustNoErr(t, err, "an unloader")

	rec := postJSON(t, h.apiUpdateLoader, "/api/loader/update", map[string]any{"id": id, "name": "DRW-UNL",
		"inbound_source": src.Name, "drain_states": []string{"unverified", "leftover"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("tick two boxes: %d %s", rec.Code, rec.Body.String())
	}
	want := []string{"leftover", "unverified"}
	if got := listedDrains(t, h, id); !slices.Equal(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	rec = postJSON(t, h.apiUpdateLoader, "/api/loader/update", map[string]any{"id": id, "name": "DRW-UNL-2",
		"inbound_source": src.Name, "auto_push": true, "drain_states": want})
	if rec.Code != http.StatusOK {
		t.Fatalf("another setting: %d %s", rec.Code, rec.Body.String())
	}
	if got := listedDrains(t, h, id); !slices.Equal(got, want) {
		t.Errorf("after saving another setting: listed %v, want %v", got, want)
	}
	rec = postJSON(t, h.apiUpdateLoader, "/api/loader/update", map[string]any{"id": id, "name": "DRW-UNL-2",
		"inbound_source": src.Name, "fed_directly": true, "drain_states": want})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("fed directly with boxes on: %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	got, err := db.GetLoader(id)
	testutil.MustNoErr(t, err, "re-read")
	if got.FedDirectly || got.InboundSource != src.Name {
		t.Errorf("the refused save landed: %+v", got)
	}
}
