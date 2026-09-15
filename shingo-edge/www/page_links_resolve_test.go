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
// window.location = (assignment, not the .href member) and window.open( are
// the remaining JS navigation forms; action= is the form-POST half of the
// same class — a form can 404 just as a link can.
//
// THE TRAILING \s* IS HOISTED OUT OF THE ALTERNATION so it applies to every
// form. Inside it, only the branch that carried it was space-tolerant: with
// the \s* attached to window.location alone, `location.href = "/x"` stopped
// matching, because the space after = was left for ["']? to eat and it
// cannot. TestPageLinkRe_ReadsEveryNavigationForm/location.href_member is
// that case, and it goes red if the \s* is pushed back inside.
var pageLinkRe = regexp.MustCompile(
	`(?:href=|action=|window\.open\(|location\.(?:href|assign|replace)\s*[=(]|window\.location\s*=)\s*["']?(/[^"'` + "`" + `\s>]*)`)

// TestPageLinkRe_ReadsEveryNavigationForm pins the scan itself, because the
// test below CANNOT.
//
// The tree's window.location and action= targets (shingoedge.js:503 →
// /orders, login.html:10 → /login) are both also reached by an href
// elsewhere, so every form after href= could stop matching tomorrow and the
// route check would stay green on the same 12 distinct paths. A scan that
// silently narrows is the failure this file exists to catch, so the forms are
// pinned here on fixed input rather than on whatever the tree happens to
// contain.
//
// The data-action negative is the reason action= is safe to scan for at all:
// this codebase is full of data-action="someHandlerName", and every one of
// them contains the literal `action="`. They are excluded by the capture
// requiring a leading slash, not by the alternation — so if the capture is
// ever loosened, this is the case that goes red.
func TestPageLinkRe_ReadsEveryNavigationForm(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		form string
		src  string
		want string
	}{
		{"href double-quoted", `<a href="/orders">`, "/orders"},
		{"href single-quoted", `<a href='/orders'>`, "/orders"},
		{"href JS concatenation", `'<a href="/operator/station/' + st.id + '">'`, "/operator/station/"},
		{"form action", `<form method="post" action="/login">`, "/login"},
		{"window.location assignment", `window.location = '/orders';`, "/orders"},
		{"window.location no spaces", `window.location='/orders';`, "/orders"},
		{"location.href member", `location.href = "/production";`, "/production"},
		{"location.assign", `location.assign('/config');`, "/config"},
		{"location.replace", `location.replace('/login');`, "/login"},
		{"window.open", `window.open('/diagnostics');`, "/diagnostics"},
	} {
		t.Run(tc.form, func(t *testing.T) {
			m := pageLinkRe.FindStringSubmatch(tc.src)
			if m == nil {
				t.Fatalf("%s: no match in %q — this navigation form is invisible to the scan", tc.form, tc.src)
			}
			if got := normalizeLinkPath(m[1]); got != tc.want {
				t.Errorf("%s: got %q, want %q", tc.form, got, tc.want)
			}
		})
	}

	// data-action handler names are not paths and must not be scanned as one.
	for _, src := range []string{
		`<button data-action="cancelProcessChangeover">`,
		`<select data-action-change="navigateToProcess">`,
	} {
		if m := pageLinkRe.FindStringSubmatch(src); m != nil {
			t.Errorf("data-action false positive: %q captured %q", src, m[1])
		}
	}
}

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
