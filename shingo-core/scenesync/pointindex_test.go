package scenesync

import (
	"slices"
	"strings"
	"testing"

	"shingocore/store/scene"
)

// pointIndexScene is a synthetic stored scene: LM7 → LM31 → AP32 → CP40 as
// edges, one point (PP50) that no edge touches, a bin location (ST7) standing
// on LM7, two names that share the number 12 (LM12 and AP12), and LM31 stored
// again in a second area. Every id and name is invented.
func pointIndexScene() *PointIndex {
	edge := func(area, from, to string) *scene.Edge {
		return &scene.Edge{AreaName: area, FromName: from, ToName: to}
	}
	return NewPointIndex(
		[]*scene.Point{
			{AreaName: "area-1", InstanceName: "PP50", ClassName: "ParkPoint"},
			{AreaName: "area-1", InstanceName: "ST7", ClassName: "GeneralLocation", PointName: "LM7"},
			{AreaName: "area-1", InstanceName: "AP12", ClassName: "ActionPoint"},
		},
		[]*scene.Edge{
			edge("area-1", "LM7", "LM31"),
			edge("area-1", "LM31", "AP32"),
			edge("area-1", "AP32", "CP40"),
			edge("area-1", "LM12", "LM7"),
			edge("area-2", "LM31", "LM7"),
		},
	)
}

// TestPointIndex_Resolve pins D5: a route's bare ids become the scene's names
// in order, and an id that names no point, or names two, leaves the robot
// with no route at all rather than a route with a hole in it.
func TestPointIndex_Resolve(t *testing.T) {
	t.Parallel()
	ix := pointIndexScene()
	for _, tc := range []struct {
		name    string
		ids     []string
		want    []string
		bad     string
		explain string // substring of Explain(bad)
	}{
		{name: "names in order, across classes", ids: []string{"7", "31", "32", "40"},
			want: []string{"LM7", "LM31", "AP32", "CP40"}},
		{name: "a point no edge touches still names", ids: []string{"50"}, want: []string{"PP50"}},
		{name: "a bin location on a point does not clash with it", ids: []string{"31", "7"},
			want: []string{"LM31", "LM7"}},
		{name: "two names share the number: no route", ids: []string{"7", "12", "31"},
			bad: "12", explain: "AP12, LM12"},
		{name: "unknown id: no route", ids: []string{"7", "31", "99"},
			bad: "99", explain: `no scene point is numbered "99"`},
		{name: "digits compare as written", ids: []string{"07"},
			bad: "07", explain: `numbered "07"`},
		{name: "the first bad id is the one reported", ids: []string{"98", "12"},
			bad: "98", explain: `numbered "98"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names, bad, ok := ix.Resolve(tc.ids)
			if !slices.Equal(names, tc.want) {
				t.Errorf("Resolve(%v) names = %v, want %v", tc.ids, names, tc.want)
			}
			if ok != (tc.bad == "") || bad != tc.bad {
				t.Errorf("Resolve(%v) bad, ok = %q, %v, want %q, %v", tc.ids, bad, ok, tc.bad, tc.bad == "")
			}
			if tc.bad != "" {
				if got := ix.Explain(bad); !strings.Contains(got, tc.explain) {
					t.Errorf("Explain(%q) = %q, want it to say %q", bad, got, tc.explain)
				}
			}
		})
	}
}

// An idle robot has nothing to resolve and nothing to report; a Core with no
// synced scene can name nothing and says so, rather than calling every id
// unknown.
func TestPointIndex_EmptyRouteAndNoScene(t *testing.T) {
	t.Parallel()
	if names, bad, ok := pointIndexScene().Resolve(nil); names != nil || bad != "" || !ok {
		t.Errorf("empty route: got %v, %q, %v, want nil, \"\", true", names, bad, ok)
	}
	for name, ix := range map[string]*PointIndex{"nil": nil, "empty": NewPointIndex(nil, nil)} {
		names, bad, ok := ix.Resolve([]string{"7", "31"})
		if names != nil || bad != "7" || ok {
			t.Errorf("%s index: got %v, %q, %v, want nil, \"7\", false", name, names, bad, ok)
		}
		if got := ix.Explain(bad); !strings.Contains(got, "no synced scene") {
			t.Errorf("%s index: Explain = %q, want it to say there is no synced scene", name, got)
		}
	}
}
