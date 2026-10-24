//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
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

// TestLoaderUpdate_TickingABoxDrainsAtOnce: an empty stands in an unloader's
// inbound source and its window is free. The save that ticks "drain empty
// bins" moves the cart to the window at once, with no other event: no window
// freeing, no arrival, no restart. A save that leaves every box off drains
// nothing. (DECISIONS D14.12, U.2.)
func TestLoaderUpdate_TickingABoxDrainsAtOnce(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	grp, err := db.CreateNodeGroup("DRT-PRESS-OUT")
	testutil.MustNoErr(t, err, "the inbound source")
	testdb.MarkGroup(t, db, grp)
	slot := &nodes.Node{Name: "DRT-PRESS-OUT-1", Enabled: true, ParentID: &grp}
	testutil.MustNoErr(t, db.CreateNode(slot), "its slot")
	window := &nodes.Node{Name: "DRT-W1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(window), "the window")
	src, err := db.GetNode(grp)
	testutil.MustNoErr(t, err, "read the source")
	id, err := db.CreateLoader(loaders.Loader{Name: "DRT-UNL", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator, InboundSource: src.Name})
	testutil.MustNoErr(t, err, "an unloader")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: window.ID, SortOrder: 1}), "its window")
	cart := &bins.Bin{BinTypeID: sd.BinType.ID, Label: "DRT-EMPTY", NodeID: &slot.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(cart), "an empty in the source")

	drainMoves := func() []string {
		t.Helper()
		rows, err := db.Query(`SELECT delivery_node FROM orders WHERE edge_uuid LIKE 'core-dr-%' AND bin_id = $1`, cart.ID)
		testutil.MustNoErr(t, err, "the drain's moves")
		defer rows.Close()
		var out []string
		for rows.Next() {
			var at string
			testutil.MustNoErr(t, rows.Scan(&at), "scan")
			out = append(out, at)
		}
		testutil.MustNoErr(t, rows.Err(), "rows")
		return out
	}
	save := func(states []string) {
		t.Helper()
		rec := postJSON(t, h.apiUpdateLoader, "/api/loader/update", map[string]any{"id": id, "name": "DRT-UNL",
			"inbound_source": src.Name, "drain_states": states})
		if rec.Code != http.StatusOK {
			t.Fatalf("save %v: %d %s", states, rec.Code, rec.Body.String())
		}
	}
	save([]string{})
	if got := drainMoves(); len(got) != 0 {
		t.Fatalf("a save with every box off made drain moves %v", got)
	}
	save([]string{"empty"})
	if got := drainMoves(); len(got) != 1 || got[0] != window.Name {
		t.Errorf("ticking the empty box made drain moves %v, want one to %s at once", got, window.Name)
	}
}
