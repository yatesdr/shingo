package www

import (
	"bytes"
	"html/template"
	"io/fs"
	"strings"
	"testing"

	"shingocore/domain"
)

// Station display names — the rendering half of the identity/label split.
//
// THE INVARIANT UNDER TEST: a station's rendered name can change while the
// stored row does not. Before v66 the label and the identity were one column,
// so renaming a station rewrote the key under orders, mission_telemetry,
// outbox, node_stations, cell_targets and the Edge's backup manifest — "a plant
// stop caused by typing in a text box" (store/registry/registry.go:345-350).
// The split only pays for itself if nothing ever copies the label onto a data
// row, and the way that regression would arrive is somebody "helpfully"
// denormalising display_name to save a lookup.
//
// This test is deliberately database-free so it runs in the default gate rather
// than behind //go:build docker. It renders the REAL orders.html against a
// hand-built historical order and swaps the resolver's answer underneath it,
// which is exactly what a rename does. The database-backed half — that the
// rename writes one column of one table and nothing else — is in
// service/station_names_test.go.

// fakeNamer is a stationNamer whose answers a test can change mid-flight, which
// is the point: a rename is a change of answer, not a change of row.
type fakeNamer struct{ byUID map[string]string }

func (f *fakeNamer) StationName(station string) string {
	if station == "" {
		return ""
	}
	if n := f.byUID[station]; n != "" {
		return n
	}
	return station
}

// renderPageWithNamer builds a page the way router.go does, with a resolver
// bound in. Mirrors renderPage (phase6_pages_render_test.go), which hardcodes a
// nil namer.
func renderPageWithNamer(t *testing.T, page string, namer stationNamer, data map[string]any) string {
	t.Helper()

	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	var found bool
	for _, p := range pages {
		if p == "templates/"+page {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s is not discovered by router.go's glob, so it is not routable", page)
	}

	base := template.New("").Funcs(templateFuncs(namer))
	base = template.Must(base.ParseFS(templateFS,
		"templates/layout.html", "templates/partials/*.html"))
	clone := template.Must(base.Clone())
	clone = template.Must(clone.ParseFS(templateFS, "templates/"+page))

	if data["Authenticated"] == nil {
		data["Authenticated"] = true
	}
	var buf bytes.Buffer
	if err := clone.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("render %s: %v", page, err)
	}
	return buf.String()
}

// TestStationRename_HistoricalOrderRendersNewName_StoredRowUntouched is THE
// test. If only one test in this change survives, it should be this one.
//
// It renders one historical order twice. Between the renders nothing about the
// order changes — it is the same value, with the same StationID — and only the
// resolver's answer differs. Both renders are asserted, and so is the order's
// own field afterwards: a rename that worked by rewriting the row would pass
// the first two assertions and fail the third.
func TestStationRename_HistoricalOrderRendersNewName_StoredRowUntouched(t *testing.T) {
	t.Parallel()

	const uid = "plant-a.line-1"
	const before = "SPRINGFIELD / LINE 1"
	const after = "SPRINGFIELD / WELD CELL"

	// A finished order from months ago. It stores the KEY, which is the whole
	// premise: the label was never written here to begin with.
	historical := &domain.Order{ID: 4711, EdgeUUID: "e-4711", StationID: uid}
	data := map[string]any{"Page": "orders", "Orders": []*domain.Order{historical}}

	namer := &fakeNamer{byUID: map[string]string{uid: before}}

	first := renderPageWithNamer(t, "orders.html", namer, data)
	if !strings.Contains(first, before) {
		t.Fatalf("first render does not show the display name %q", before)
	}

	// The rename. One map entry, standing in for one UPDATE of one column.
	namer.byUID[uid] = after

	second := renderPageWithNamer(t, "orders.html", namer, data)
	if !strings.Contains(second, after) {
		t.Fatalf("after the rename the historical order still does not show %q — "+
			"a past record must pick up the new name, since nothing stored the old one", after)
	}
	if strings.Contains(second, before) {
		t.Errorf("the old name %q survives in the render after a rename", before)
	}

	// THE ASSERTION THAT MAKES THE OTHER TWO MEAN ANYTHING. The rendered output
	// changed; the record did not. A denormalising implementation — one that
	// wrote display_name onto the order to avoid a lookup — would have had to
	// mutate this field to change the render.
	if historical.StationID != uid {
		t.Fatalf("the order row's StationID changed to %q — renaming must NEVER "+
			"touch a stored station value; that is the v66 regression this split exists to prevent",
			historical.StationID)
	}
	if strings.Contains(historical.StationID, after) || strings.Contains(historical.StationID, before) {
		t.Fatalf("a display name leaked into the stored StationID: %q", historical.StationID)
	}
}

// TestStationName_FallsBackToTheRawIdentity covers the values that have no
// registry row and never will.
//
// Measured in production at both plants: core-operator, core-direct and core-test
// are Core's own synthetic order sources, and '*' is the broadcast address on
// 884 outbox rows at Springfield. None of them is an enrolled edge. Rendering
// them blank would erase information from a screen; erroring would take the
// page down. Rendering them as themselves is today's behaviour and is correct.
func TestStationName_FallsBackToTheRawIdentity(t *testing.T) {
	t.Parallel()

	namer := &fakeNamer{byUID: map[string]string{"plant-a.line-1": "SPRINGFIELD / LINE 1"}}

	for _, station := range []string{"core-operator", "core-direct", "core-test", "*", "stn-never-enrolled"} {
		t.Run(station, func(t *testing.T) {
			order := &domain.Order{ID: 1, EdgeUUID: "e-1", StationID: station}
			out := renderPageWithNamer(t, "orders.html", namer,
				map[string]any{"Page": "orders", "Orders": []*domain.Order{order}})
			if !strings.Contains(out, station) {
				t.Fatalf("station %q with no registry row did not render as itself", station)
			}
		})
	}
}

// TestStationNameFunc_NilNamerRendersRawIdentity pins the no-resolver case.
//
// templateFuncs(nil) is what the parse-only tests use, and it must produce the
// same output as an unknown station rather than a blank cell — one fallback
// path, not two.
func TestStationNameFunc_NilNamerRendersRawIdentity(t *testing.T) {
	t.Parallel()

	fn, ok := templateFuncs(nil)["stationName"].(func(string) string)
	if !ok {
		t.Fatalf("stationName is not a func(string) string")
	}
	if got := fn("plant-a.line-1"); got != "plant-a.line-1" {
		t.Fatalf("nil namer: stationName(%q) = %q, want the identity back", "plant-a.line-1", got)
	}
	if got := fn(""); got != "" {
		t.Fatalf("nil namer: stationName(\"\") = %q, want \"\"", got)
	}
}

// TestStationName_IsUidFormatAgnostic pins that resolution is a lookup.
//
// Whether the two live plants keep their backfilled 'plant-a.line-1' or take
// fresh minted uids is an open decision. This asserts the rendering path does
// not care: same behaviour for both shapes, and for a third that resembles
// neither. A prefix check or a split on '.' would pass the first case and fail
// one of the others.
func TestStationName_IsUidFormatAgnostic(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"plant-a.line-1":         "LEGACY BACKFILLED",
		"stn-a1b2c3d4e5f6a7b8":   "MINTED OPAQUE",
		"edge-a1b2c3d4e5f6a7b8":  "MINTED ALTERNATE",
		"anything at all/really": "ARBITRARY",
	}
	namer := &fakeNamer{byUID: cases}

	for uid, want := range cases {
		order := &domain.Order{ID: 1, EdgeUUID: "e-1", StationID: uid}
		out := renderPageWithNamer(t, "orders.html", namer,
			map[string]any{"Page": "orders", "Orders": []*domain.Order{order}})
		if !strings.Contains(out, want) {
			t.Errorf("uid %q did not resolve to %q — resolution must be a whole-string "+
				"lookup with no assumption about the identifier's shape", uid, want)
		}
	}
}
