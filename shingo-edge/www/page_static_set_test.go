package www

import (
	"html/template"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"testing"

	"shingo/shared/assets"
	"shingo/shared/assets/assetcheck"
)

// page_static_set_test.go — every page's static set, loaded as a browser
// loads it, from every page template.
//
// THE GATES. For each page: every module it can reach (static imports and
// import()) is in its import map, so a browser with maps fetches every module
// by its hashed URL, cached for good; a browser without import maps (Chromium
// before 89; the kiosks' versions are unverified) loads the same modules,
// each by its plain URL, revalidated by ETag to a 304; neither loads one
// module under two URLs; no *.test.js is reached; and the cold boot stays
// inside the page's budget below. assetcheck.Skeleton renders the template
// with no data and every branch taken, so a page that needs a database is
// checked too; TestStaticAssets_EveryTagThePagesNameIsServedAndCompressed
// loads the two kiosk-facing pages as the real router renders them.
//
// A BUDGET IS A CEILING, NOT A GOLDEN. Each row is the boot measured when it
// was set — requests, then body bytes as sent (gzip where held) — with two
// requests and about a tenth in bytes of room, so a JS change elsewhere does
// not fail here and a page that gains a module or a library does. Raise a
// row in the commit that grows the page, and say why there.
var edgePageBudgets = map[string]struct {
	requests int
	bytes    int64
}{
	// measured 2026-11-11 at 810a435e (requests, bytes)
	"changeover.html":       {18, 82 << 10},  // 16, 75886
	"config.html":           {19, 74 << 10},  // 17, 68541
	"containment.html":      {18, 62 << 10},  // 16, 57521
	"diagnostics.html":      {16, 53 << 10},  // 14, 49031
	"login.html":            {15, 51 << 10},  // 13, 47427
	"manual-message.html":   {19, 73 << 10},  // 17, 67448 at s3-adapters: the shared picker and its adapters
	"manual-order.html":     {20, 75 << 10},  // 18, 68974 at s3-adapters: the shared picker and its adapters
	"operator-display.html": {27, 194 << 10}, // 25, 180258
	"orders.html":           {18, 80 << 10},  // 16, 73607
	"processes.html":        {32, 340 << 10}, // 30, 315704
	"production.html":       {22, 101 << 10}, // 20, 93782 at s3-adapters: the shared picker and its adapters
	"replenishment.html":    {17, 60 << 10},  // 15, 55738
}

var (
	hxAttrRe      = regexp.MustCompile(`\shx-[a-z-]+=`)
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
)

// edgePageSet is the one template set NewRouter parses.
func edgePageSet(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("").Funcs(templateFuncs()).
		ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html"))
}

func TestPages_StaticSetLoadsInEveryBrowser(t *testing.T) {
	t.Parallel()
	set := edgePageSet(t)
	h := staticAssets()
	files, err := fs.Glob(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	pages := map[string]bool{}
	for _, f := range files {
		name := path.Base(f)
		if name == "header.html" || name == "footer.html" {
			continue // drawn inside every page, not one
		}
		pages[name] = true
		html, err := assetcheck.Skeleton(set, name, h)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		assertStaticSet(t, name, html, edgePageBudgets[name].requests, edgePageBudgets[name].bytes)
		// HTMX WHERE IT SWAPS, NOWHERE ELSE: a page loads htmx.min.js (16 kB
		// gzipped) exactly when its markup, partials included, carries an hx-
		// attribute — today changeover, orders and production.
		swaps := hxAttrRe.MatchString(htmlCommentRe.ReplaceAllString(html, ""))
		loads := false
		for _, f := range assetcheck.Load(staticAssets(), html, assetcheck.WithImportMap).Fetches {
			if p, _ := assets.PlainURL(f.URL); p == "/static/js/htmx.min.js" {
				loads = true
			}
		}
		if swaps != loads {
			t.Errorf("%s: carries hx- attributes %v, loads htmx %v: htmx goes on the pages that swap and only those",
				name, swaps, loads)
		}
	}
	for name := range edgePageBudgets {
		if !pages[name] {
			t.Errorf("a budget for %s, which is not a page template: delete the row", name)
		}
	}
}

// assertStaticSet loads html in both browsers and holds it to the gates; a
// zero budget asks for one.
func assertStaticSet(t *testing.T, label, html string, maxRequests int, maxBytes int64) {
	t.Helper()
	h := staticAssets()
	with := assetcheck.Load(h, html, assetcheck.WithImportMap)
	without := assetcheck.Load(h, html, assetcheck.NoImportMap)
	for _, r := range []struct {
		mode assetcheck.Mode
		res  assetcheck.Result
	}{{assetcheck.WithImportMap, with}, {assetcheck.NoImportMap, without}} {
		for _, p := range r.res.Problems {
			t.Errorf("%s (%s): %s", label, r.mode, p)
		}
	}
	if len(with.Modules()) == 0 {
		t.Errorf("%s loads no module: every Edge page loads its hub, so the tag reader has drifted", label)
	}
	if !slices.Equal(with.Modules(), without.Modules()) {
		t.Errorf("%s: the module set differs by browser:\n  with a map %v\n  without    %v",
			label, with.Modules(), without.Modules())
	}
	n, b := with.Boot()
	t.Logf("%s: cold boot %d requests, %d bytes; %d modules reachable", label, n, b, len(with.Modules()))
	switch {
	case maxRequests == 0:
		t.Errorf("%s has no budget: its cold boot is %d requests, %d bytes; add a row", label, n, b)
	case n > maxRequests || b > maxBytes:
		t.Errorf("%s: cold boot %d requests, %d bytes, over its budget of %d, %d", label, n, b, maxRequests, maxBytes)
	}
}
