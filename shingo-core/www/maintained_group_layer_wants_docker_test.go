//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// maintained_group_layer_wants_docker_test.go — a level's wanted heights ride
// the level's own save and go with the level's remove (the Nodes page's
// maintained-group section). A height is wanted only of a type that states its
// totes per layer, and 0 layers is the tops & bottoms row.

// layerWantsOf reads a group's wanted heights through the editor's GET, as
// "TYPE:layers=want" in the answer's order.
func layerWantsOf(t *testing.T, h *Handlers, grpID int64) []string {
	t.Helper()
	rec := getPlain(t, h.apiMaintainedGroup, fmt.Sprintf("/api/nodes/maintained-group?id=%d", grpID))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET maintained group: status %d; body=%s", rec.Code, rec.Body.String())
	}
	var cfg struct {
		LayerWants []store.MaintainLayerWant `json:"layer_wants"`
	}
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &cfg), "decode maintained group")
	out := []string{}
	for _, w := range cfg.LayerWants {
		out = append(out, fmt.Sprintf("%s:%d=%d", w.BinTypeCode, w.Layers, w.Want))
	}
	return out
}

func TestMaintainedGroupLevel_LayerWants(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	grpID, err := db.CreateNodeGroup("GRP-MGLW")
	testutil.MustNoErr(t, err, "create group")
	testdb.MarkGroup(t, db, grpID)
	for i := 1; i <= 6; i++ {
		testutil.MustNoErr(t, db.CreateNode(&nodes.Node{
			Name: fmt.Sprintf("GRP-MGLW-P%d", i), ParentID: &grpID, Enabled: true,
		}), "create position")
	}
	tote := mintBinType(t, db, "MGLW-TOTE").ID
	small := mintBinType(t, db, "MGLW-SMALL").ID
	kd := mintBinType(t, db, "MGLW-KD").ID
	_, err = db.Exec(`UPDATE bin_types SET totes_per_layer = 8 WHERE id IN ($1, $2)`, tote, small)
	testutil.MustNoErr(t, err, "the tote types")

	saveLevel := func(binTypeID int64, want int, layerWants any) int {
		body := map[string]any{"group_node_id": grpID, "bin_type_id": binTypeID, "want": want}
		if layerWants != nil {
			body["layer_wants"] = layerWants
		}
		rec := postJSON(t, h.apiMaintainedGroupLevelSet, "/api/nodes/maintained-group/level", body)
		if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest && rec.Code != http.StatusConflict {
			t.Fatalf("level save: status %d; body=%s", rec.Code, rec.Body.String())
		}
		return rec.Code
	}
	want := func(step string, got, want []string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: layer wants = %v, want %v", step, got, want)
		}
	}

	saveLevel(tote, 3, []map[string]int{{"layers": 5, "want": 1}, {"layers": 3, "want": 2}})
	saveLevel(kd, 2, nil)
	want("saved with the level, in height order", layerWantsOf(t, h, grpID),
		[]string{"MGLW-TOTE:3=2", "MGLW-TOTE:5=1"})

	saveLevel(tote, 4, nil)
	want("a level save with no layer_wants leaves them standing", layerWantsOf(t, h, grpID),
		[]string{"MGLW-TOTE:3=2", "MGLW-TOTE:5=1"})

	saveLevel(tote, 4, []map[string]int{{"layers": 4, "want": 0}})
	want("a list replaces them, a want of 0 kept", layerWantsOf(t, h, grpID),
		[]string{"MGLW-TOTE:4=0"})

	saveLevel(tote, 4, []map[string]int{{"layers": 0, "want": 1}, {"layers": 4, "want": 0}})
	want("0 layers is the tops & bottoms row", layerWantsOf(t, h, grpID),
		[]string{"MGLW-TOTE:0=1", "MGLW-TOTE:4=0"})
	saveLevel(tote, 4, []map[string]int{{"layers": 4, "want": 0}})

	for _, bad := range []any{
		[]map[string]int{{"layers": -1, "want": 1}},
		[]map[string]int{{"layers": 2, "want": -1}},
		[]map[string]int{{"layers": 2, "want": 1}, {"layers": 2, "want": 3}},
	} {
		if code := saveLevel(tote, 4, bad); code != http.StatusBadRequest {
			t.Errorf("layer_wants %v: status %d, want 400", bad, code)
		}
	}
	want("a refused list writes nothing", layerWantsOf(t, h, grpID), []string{"MGLW-TOTE:4=0"})

	saveLevel(tote, 4, []map[string]int{})
	want("an empty list clears them", layerWantsOf(t, h, grpID), []string{})

	if code := saveLevel(kd, 2, []map[string]int{{"layers": 1, "want": 1}}); code != http.StatusConflict {
		t.Errorf("a height on a type that states no totes per layer: status %d, want 409", code)
	}
	if code := saveLevel(kd, 2, []map[string]int{{"layers": 0, "want": 1}}); code != http.StatusConflict {
		t.Errorf("tops & bottoms on a type that states no totes per layer: status %d, want 409", code)
	}
	want("a refused type writes nothing", layerWantsOf(t, h, grpID), []string{})

	saveLevel(tote, 4, []map[string]int{{"layers": 3, "want": 2}})
	saveLevel(small, 1, []map[string]int{{"layers": 1, "want": 1}})
	rec := postJSON(t, h.apiMaintainedGroupLevelRemove, "/api/nodes/maintained-group/level/remove",
		map[string]any{"group_node_id": grpID, "bin_type_id": tote})
	if rec.Code != http.StatusOK {
		t.Fatalf("level remove: status %d; body=%s", rec.Code, rec.Body.String())
	}
	want("removing the level removes its heights and no other type's", layerWantsOf(t, h, grpID),
		[]string{"MGLW-SMALL:1=1"})

	levels, err := db.ListMaintainLevels(grpID)
	testutil.MustNoErr(t, err, "ListMaintainLevels")
	if len(levels) != 2 || levels[0].BinTypeID != kd || levels[0].Want != 2 || levels[1].BinTypeID != small || levels[1].Want != 1 {
		t.Errorf("levels after the remove = %+v, want KD at 2 and SMALL at 1: the layer wants change no level", levels)
	}
}

// The Nodes page renders its maintained-group section, the wanted heights'
// hint beside the level, for a signed-in viewer; the heights themselves are
// drawn by nodes-maintain.js from the GET above.
func TestMaintainedGroupLevel_NodesPageRenders(t *testing.T) {
	t.Parallel()
	h, _ := testHandlersForPages(t)
	req := httptest.NewRequest(http.MethodGet, "/nodes", nil)
	req.AddCookie(loginCookie(t, h))
	rec := httptest.NewRecorder()
	h.handleNodes(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("nodes page status %d; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`id="nf-maintain-levels"`, "the heights wanted here tell the unloader"} {
		if !strings.Contains(body, want) {
			t.Errorf("nodes page missing %q", want)
		}
	}
}
