package www

import (
	"html/template"
	"io/fs"
	"path"
	"slices"
	"strings"
	"testing"

	"shingo/shared/assets/assetcheck"
)

// page_static_set_test.go — every page's static set, loaded as a browser
// loads it, from every page template.
//
// THE GATES. For each page: every module it can reach (static imports and
// import()) is in its import map, so a browser with maps fetches every module
// by its hashed URL, cached for good; a browser without import maps (Chromium
// before 89) loads the same modules, each by its plain URL, revalidated by
// ETag to a 304; neither loads one module under two URLs; no *.test.js is
// reached; and the cold boot stays inside the page's budget below.
// assetcheck.Skeleton renders the page as router.go builds it (layout and
// partials, the page cloned over them) with no data and every branch taken,
// so no database is needed to check a page that needs one to render.
//
// A BUDGET IS A CEILING, NOT A GOLDEN. Each row is the boot measured when it
// was set — requests, then body bytes as sent (gzip where held) — with two
// requests and about a tenth in bytes of room, so a JS change elsewhere does
// not fail here and a page that gains a module or a library does. Raise a
// row in the commit that grows the page, and say why there.
var corePageBudgets = map[string]struct {
	requests int
	bytes    int64
}{
	// measured 2026-11-11 (requests, bytes); the skeleton counts sim-speed-strip.js,
	// which only a sim build loads
	"bins.html":                  {22, 95 << 10},  // 20, 88332
	"config.html":                {20, 82 << 10},  // 18, 75732
	"count-history.html":         {20, 82 << 10},  // 18, 75485
	"cycle-time.html":            {18, 70 << 10},  // 16, 64923
	"dashboard-display.html":     {11, 26 << 10},  // 9, 24142
	"dashboard-frame.html":       {18, 70 << 10},  // 16, 64923
	"dashboard-map.html":         {11, 65 << 10},  // 9, 60308
	"dashboard-node-report.html": {10, 41 << 10},  // 8, 38005
	"dashboard.html":             {19, 80 << 10},  // 17, 74069
	"demand-episode.html":        {18, 70 << 10},  // 16, 64923
	"demand-episodes.html":       {18, 70 << 10},  // 16, 64923
	"diagnostics.html":           {19, 76 << 10},  // 17, 70460
	"edges.html":                 {19, 72 << 10},  // 17, 66518
	"heartbeat.html":             {13, 59 << 10},  // 11, 54519
	"inventory.html":             {20, 82 << 10},  // 18, 75853
	"login.html":                 {18, 70 << 10},  // 16, 64923
	"material-flags.html":        {18, 70 << 10},  // 16, 64923
	"mission-detail.html":        {21, 83 << 10},  // 19, 76534
	"missions.html":              {24, 172 << 10}, // 22, 160092
	"nodes.html":                 {23, 135 << 10}, // 21, 125291
	"orders.html":                {22, 96 << 10},  // 20, 88677
	"orphans.html":               {18, 70 << 10},  // 16, 64923
	"overview.html":              {32, 184 << 10}, // 30, 171062
	"payloads.html":              {19, 82 << 10},  // 17, 75498
	"rds_explorer.html":          {19, 84 << 10},  // 17, 77388
	"robots.html":                {26, 116 << 10}, // 24, 107592
	"sourcing.html":              {19, 77 << 10},  // 17, 71200
	"test-orders.html":           {19, 76 << 10},  // 17, 70321
}

func TestPages_StaticSetLoadsInEveryBrowser(t *testing.T) {
	h := staticAssets()
	base := template.Must(template.New("").Funcs(templateFuncs(nil)).
		ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	pages := map[string]bool{}
	for _, f := range files {
		name := path.Base(f)
		if name == "layout.html" {
			continue
		}
		pages[name] = true
		set := template.Must(template.Must(base.Clone()).ParseFS(templateFS, f))
		// A page that writes its own document renders by its own name
		// (renderBare); every other one through the layout.
		entry := "layout"
		if strings.Contains(set.Lookup(name).Tree.Root.String(), "<html") {
			entry = name
		}
		html, err := assetcheck.Skeleton(set, entry, h)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		with := assetcheck.Load(h, html, assetcheck.WithImportMap)
		without := assetcheck.Load(h, html, assetcheck.NoImportMap)
		for _, r := range []struct {
			mode assetcheck.Mode
			res  assetcheck.Result
		}{{assetcheck.WithImportMap, with}, {assetcheck.NoImportMap, without}} {
			for _, p := range r.res.Problems {
				t.Errorf("%s (%s): %s", name, r.mode, p)
			}
		}
		if len(with.Modules()) == 0 {
			t.Errorf("%s loads no module: every Core page loads app.js or its own, so the tag reader has drifted", name)
		}
		if !slices.Equal(with.Modules(), without.Modules()) {
			t.Errorf("%s: the module set differs by browser:\n  with a map %v\n  without    %v",
				name, with.Modules(), without.Modules())
		}
		n, b := with.Boot()
		t.Logf("%s: cold boot %d requests, %d bytes; %d modules reachable", name, n, b, len(with.Modules()))
		switch budget, ok := corePageBudgets[name]; {
		case !ok:
			t.Errorf("%s has no budget: its cold boot is %d requests, %d bytes; add a row", name, n, b)
		case n > budget.requests || b > budget.bytes:
			t.Errorf("%s: cold boot %d requests, %d bytes, over its budget of %d, %d",
				name, n, b, budget.requests, budget.bytes)
		}
	}
	for name := range corePageBudgets {
		if !pages[name] {
			t.Errorf("a budget for %s, which is not a page template: delete the row", name)
		}
	}
}
