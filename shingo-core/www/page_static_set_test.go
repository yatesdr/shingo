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
	// measured 2026-11-11 at 810a435e (requests, bytes); the skeleton counts sim-speed-strip.js,
	// which only a sim build loads
	"bins.html":                  {28, 125 << 10}, // 26, 115627 at the live table: bins-live.js, the feed that keeps the rows current (+ the deviceconfig tab's ~1.4KB tokens.css growth rides inside this row); 23, 102252 at s3-adapters: the shared picker and its adapters
	"config.html":                {20, 83 << 10},  // 18, 76960
	"count-history.html":         {20, 83 << 10},  // 18, 76713
	"cycle-time.html":            {18, 72 << 10},  // 16, 66151
	"dashboard-display.html":     {11, 27 << 10},  // 9, 24489
	"dashboard-frame.html":       {18, 72 << 10},  // 16, 66151
	"dashboard-map.html":         {18, 87 << 10},  // 16, 79801 at the shared-map rebase of sm/u10-panel: scene-rows.js (U6), scene-view.css and robot-motion.js (U10); 13, 77071 at sm/u5-shared: the scene machinery in four shared modules
	"dashboard-node-report.html": {10, 42 << 10},  // 8, 38352
	"dashboard.html":             {19, 81 << 10},  // 17, 75297
	"demand-episode.html":        {18, 72 << 10},  // 16, 66151
	"demand-episodes.html":       {18, 72 << 10},  // 16, 66151
	"diagnostics.html":           {19, 78 << 10},  // 17, 71688
	"edges.html":                 {19, 73 << 10},  // 17, 67746
	"heartbeat.html":             {13, 59 << 10},  // 11, 54874
	"inventory.html":             {20, 83 << 10},  // 18, 77081
	"login.html":                 {18, 72 << 10},  // 16, 66151
	"material-flags.html":        {18, 72 << 10},  // 16, 66151
	"mission-detail.html":        {21, 84 << 10},  // 19, 77762
	"missions.html":              {24, 174 << 10}, // 22, 161320
	"nodes.html":                 {26, 151 << 10}, // 24, 140189 at s3-adapters: the shared picker and its adapters
	"orders.html":                {25, 110 << 10}, // 23, 101651 at s3-adapters: the shared picker and its adapters
	"orphans.html":               {18, 72 << 10},  // 16, 66151
	"overview.html":              {32, 186 << 10}, // 30, 172290
	"payloads.html":              {19, 83 << 10},  // 17, 76726
	"rds_explorer.html":          {19, 85 << 10},  // 17, 78632
	"robots.html":                {28, 125 << 10}, // 26, 115848 at sm/u6-consumers: the localization board on the robot map's lanes (scene-net.js, components/scene-rows.js; +2 requests, +7028 B gz); 24, 108820
	"sourcing.html":              {19, 78 << 10},  // 17, 72428
	"test-orders.html":           {22, 91 << 10},  // 20, 84246 at s3-adapters: the shared picker and its adapters
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
