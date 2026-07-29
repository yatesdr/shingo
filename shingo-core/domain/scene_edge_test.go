package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSceneEdge_JSONNamesTheMapReads pins the wire names on /api/map/edges.
//
// dashboard-map.js reads ctrl1_x/ctrl1_y/ctrl2_x/ctrl2_y off each edge and
// draws a cubic when all four are finite. Renaming a tag here is a silent
// break: the map keeps rendering, every lane comes out straight again, and no
// Go test and no browser error says so. That is the failure this whole change
// exists to undo, so the names get pinned rather than assumed.
func TestSceneEdge_JSONNamesTheMapReads(t *testing.T) {
	t.Parallel()

	c1x, c1y, c2x, c2y := -0.198, 36.401, -5.065, 36.951
	curved := SceneEdge{
		InstanceName: "LM10-LM113",
		FromX:        -0.254, FromY: 33.554, ToX: -6.572, ToY: 36.951,
		Ctrl1X: &c1x, Ctrl1Y: &c1y, Ctrl2X: &c2x, Ctrl2Y: &c2y,
	}
	b, err := json.Marshal(curved)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for name, want := range map[string]float64{
		"ctrl1_x": c1x, "ctrl1_y": c1y, "ctrl2_x": c2x, "ctrl2_y": c2y,
	} {
		v, ok := got[name]
		if !ok {
			t.Errorf("%s absent from the edge JSON; dashboard-map.js reads it by that name", name)
			continue
		}
		if f, ok := v.(float64); !ok || f != want {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}

	// A straight lane must OMIT the four keys rather than send zeros. The map
	// tests isFinite on them, and 0 is finite: a zeroed straight aisle would
	// be drawn as a cubic dragged through the origin — 8.8 m off on this one.
	straight := SceneEdge{
		InstanceName: "LM100-AP102",
		FromX:        -0.544, FromY: 11.787, ToX: 0.886, ToY: 11.807,
	}
	sb, err := json.Marshal(straight)
	if err != nil {
		t.Fatalf("marshal straight: %v", err)
	}
	for _, name := range []string{"ctrl1_x", "ctrl1_y", "ctrl2_x", "ctrl2_y"} {
		if strings.Contains(string(sb), name) {
			t.Errorf("straight aisle emitted %s; want the key omitted: %s", name, sb)
		}
	}
}

// TestSceneEdge_CurvedNeedsAllFour: three coordinates describe no cubic.
func TestSceneEdge_CurvedNeedsAllFour(t *testing.T) {
	t.Parallel()

	v := 1.0
	full := SceneEdge{Ctrl1X: &v, Ctrl1Y: &v, Ctrl2X: &v, Ctrl2Y: &v}
	if !full.Curved() {
		t.Error("all four handles set: want Curved")
	}
	if (SceneEdge{}).Curved() {
		t.Error("no handles set: want not Curved")
	}
	for i, partial := range []SceneEdge{
		{Ctrl1X: &v},
		{Ctrl1X: &v, Ctrl1Y: &v},
		{Ctrl1X: &v, Ctrl1Y: &v, Ctrl2X: &v},
		{Ctrl2X: &v, Ctrl2Y: &v},
		{Ctrl1Y: &v, Ctrl2X: &v, Ctrl2Y: &v},
	} {
		if partial.Curved() {
			t.Errorf("partial handle set %d read as Curved: %+v", i, partial)
		}
	}
}
