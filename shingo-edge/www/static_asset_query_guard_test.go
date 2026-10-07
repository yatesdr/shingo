package www

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// assetAttrRe reads every src= / href= attribute value out of a template,
// quoted either way, across a line break between the name and the value.
// ATTRIBUTES, NOT TEXT: Core's layout.html carries a literal `?v=` in a comment that
// explains why there is none, and that must not trip the guard.
var assetAttrRe = regexp.MustCompile(`(?i)\b(?:src|href)\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// TestTemplates_NoCacheBustQuery fails on a `?v=` query inside any src or href
// in a page template.
//
// Every asset tag used to carry ?v=<per-render stamp>, so every stamped asset was
// downloaded in full on every page view, and a module named by a stamped tag
// and by a bare import was two URLs and two module instances. serverInstanceETag's
// ETag already makes a bare URL fresh across a restart (router.go), so a
// query buys nothing and costs the whole file each time.
func TestTemplates_NoCacheBustQuery(t *testing.T) {
	assertNoAssetQuery(t, templatesFS, map[string]bool{
		// The HOP press stream's file; it removes its 8 tags and the cacheBust
		// func together. The ONE exemption.
		"templates/operator-display.html": true,
	})
}

func assertNoAssetQuery(t *testing.T, fsys fs.FS, exempt map[string]bool) {
	t.Helper()
	var files []string
	for _, pat := range []string{"templates/*.html", "templates/partials/*.html"} {
		m, err := fs.Glob(fsys, pat)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatal("no templates found — the guard would pass on nothing")
	}
	for _, f := range files {
		if exempt[f] {
			continue
		}
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, m := range assetAttrRe.FindAllStringSubmatchIndex(src, -1) {
			val := ""
			if m[2] >= 0 {
				val = src[m[2]:m[3]]
			} else {
				val = src[m[4]:m[5]]
			}
			if strings.Contains(val, "?v=") {
				line := strings.Count(src[:m[0]], "\n") + 1
				t.Errorf("%s:%d: %q carries a ?v= query — serve the bare URL; the ETag handler "+
					"keeps it fresh across a restart", f, line, val)
			}
		}
	}
}
