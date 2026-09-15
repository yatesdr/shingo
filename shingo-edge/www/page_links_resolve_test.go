package www

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingoedge/internal/testdb"
)

// page_links_resolve_test.go — every page URL the front-end navigates to is a
// route the router actually has.
//
// THE SIBLING OF static_assets_served_test.go, AND ITS BLIND SPOT. That test
// reads the tags out of a *served page*, which is the right shape for assets
// and the wrong one for links: a link built by concatenation in JS is never in
// anybody's HTML. processes-desktop.js wrote
//
//	'<a class="pd-dimlink" href="/operator?station=' + st.id + '">Open</a>'
//
// and /operator has never been a route on either side of the composer work —
// the operator display is /operator/station/{id}. Every station row on the new
// Processes page answered 404, and it shipped to Hopkinsville that way
// (2026-09-15). The page rendered, the table rendered, and the only trace was a
// 404 for somebody who clicked.
//
// This is the second link of this class to break with nothing catching it; the
// first was the bf5ed4b data-action breakage, whose drift test scanned HTML
// only and so could not see it either. So the scan here is over the SOURCE —
// static/**/*.js and templates/**/*.html — not over a rendered response.
//
// Asking chi's Match rather than issuing a GET is deliberate. A GET cannot tell
// "no such route" from "no such station": /operator/station/999999 is a 404 on
// a route that exists. Match answers the only question this test is asking.

// pageLinkRe reads navigation targets out of source. The capture stops at a
// quote, so the JS concatenation case yields the literal prefix
// ("/operator/station/") rather than a path with an expression glued into it.
var pageLinkRe = regexp.MustCompile(
	`(?:href=|location\.(?:href|assign|replace)\s*[=(]\s*)["']?(/[^"'` + "`" + `\s>]*)`)

func TestPageLinks_EveryLinkTheFrontEndNamesHasARoute(t *testing.T) {
	db := testdb.Open(t)
	_, router := realFlowRouter(t, db)

	found := map[string][]string{} // normalized path -> source files naming it
	for _, root := range []string{"static", "templates"} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			name := d.Name()
			// Skip the JS unit tests: a fixture URL there navigates nothing.
			if strings.HasSuffix(name, ".test.js") {
				return nil
			}
			if !strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".html") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range pageLinkRe.FindAllStringSubmatch(string(src), -1) {
				p := normalizeLinkPath(m[1])
				if p == "" {
					continue
				}
				if src := filepath.ToSlash(path); !slices.Contains(found[p], src) {
					found[p] = append(found[p], src)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(found) == 0 {
		t.Fatal("no page links found at all — either the tree moved or the pattern has drifted, " +
			"and this test would pass on both")
	}

	paths := make([]string, 0, len(found))
	for p := range found {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		if routeExists(router, p) {
			continue
		}
		t.Errorf("%s is linked from %s but no route answers it — the link 404s for whoever clicks it",
			p, strings.Join(found[p], ", "))
	}
	if !t.Failed() {
		t.Logf("%d distinct page links, all routed", len(paths))
	}
}

// normalizeLinkPath reduces a captured target to something chi can be asked
// about, and returns "" for the ones this test does not own.
func normalizeLinkPath(raw string) string {
	p := raw
	// A Go template beyond this point is conditional markup as often as it is a
	// path segment ("/processes{{if .X}}?…"), so the literal prefix is the only
	// part that can be checked without guessing which.
	if i := strings.Index(p, "{{"); i >= 0 {
		p = p[:i]
	}
	// chi matches on path; the query string is not part of the route.
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if i := strings.IndexByte(p, '#'); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimRight(p, "+ ")
	// Protocol-relative ("//host/…") is an external target, not a route here.
	if strings.HasPrefix(p, "//") {
		return ""
	}
	// Assets are static_assets_served_test.go's, and it checks more than routing.
	if strings.HasPrefix(p, "/static/") {
		return ""
	}
	if p == "" || p[0] != '/' {
		return ""
	}
	return p
}

// routeExists asks whether any registered route answers this path. A captured
// prefix is tried with a placeholder segment too: concatenation
// ("/operator/station/" + id) and template truncation both leave a prefix whose
// real route takes a parameter, and neither is a finding.
func routeExists(router *chi.Mux, p string) bool {
	candidates := []string{p}
	if strings.HasSuffix(p, "/") {
		candidates = append(candidates, p+"1")
	} else {
		candidates = append(candidates, p+"/1")
	}
	for _, c := range candidates {
		if router.Match(chi.NewRouteContext(), "GET", c) {
			return true
		}
	}
	return false
}
