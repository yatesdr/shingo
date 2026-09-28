//go:build docker

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
)

// page_links_resolve_test.go — every page URL the Core front-end navigates to
// is a route the Core router actually has. The twin of
// shingo-edge/www/page_links_resolve_test.go, ported 2026-09-26 ("one fact,
// one owner" stream): the two www surfaces share the failure class — a link
// built by concatenation in JS is never in anybody's HTML, so a scan of
// rendered pages cannot see it, and a broken one 404s only for whoever clicks
// it. Edge learned this twice (the bf5ed4b data-action breakage, then
// processes-desktop.js linking /operator on 2026-09-15, which shipped to a
// plant). Core's front-end has the same mixture of href templates, form
// actions, window.location assignments and window.open calls, and had no
// check of this shape at all.
//
// Asking chi's Match rather than issuing a GET is deliberate (same reason as
// Edge's twin): a GET cannot tell "no such route" from "no such order" —
// /missions/999999 is a 404 on a route that exists. Match answers the only
// question this test is asking.

// pageLinkRe reads navigation targets out of source. The capture stops at a
// quote, so a JS concatenation yields the literal prefix ("/missions/")
// rather than a path with an expression glued into it. window.location =
// (assignment, not the .href member) and window.open( are the remaining JS
// navigation forms; action= is the form-POST half of the same class — a form
// can 404 just as a link can.
//
// THE TRAILING \s* IS HOISTED OUT OF THE ALTERNATION so it applies to every
// form (see the Edge twin's history for the version that got this wrong:
// with the \s* attached to window.location alone, `location.href = "/x"`
// stopped matching). TestPageLinkRe_ReadsEveryNavigationForm pins that.
var pageLinkRe = regexp.MustCompile(
	`(?:href=|action=|window\.open\(|location\.(?:href|assign|replace)\s*[=(]|window\.location\s*=)\s*["']?(/[^"'` + "`" + `\s>]*)`)

// TestPageLinkRe_ReadsEveryNavigationForm pins the scan itself, because the
// walk test below CANNOT: the tree's forms overlap (login.html's /login is
// reached by href and action=), so one form could stop matching tomorrow and
// the route check would stay green on the paths the other forms still find.
// The cases use Core's own vocabulary (missions, payloads, /api/inventory/export).
func TestPageLinkRe_ReadsEveryNavigationForm(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		form string
		src  string
		want string
	}{
		{"href double-quoted", `<a href="/missions">`, "/missions"},
		{"href single-quoted", `<a href='/orphans'>`, "/orphans"},
		{"href JS concatenation", `window.location.href = '/missions/' + tr.dataset.orderId`, "/missions/"},
		{"form action", `<form method="POST" action="/config/save">`, "/config/save"},
		{"window.location assignment", `window.location = '/api/inventory/export';`, "/api/inventory/export"},
		{"window.location no spaces", `window.location='/inventory';`, "/inventory"},
		{"location.href member", `location.href = "/payloads";`, "/payloads"},
		{"location.assign", `location.assign('/cycle-time');`, "/cycle-time"},
		{"location.replace", `location.replace('/login');`, "/login"},
		{"window.open", `window.open('/', '_blank');`, "/"},
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
	// Core's app.js resolves data-action="verb:arg" attributes everywhere, so
	// this negative is what keeps action= safe to scan for at all.
	for _, src := range []string{
		`<button data-action="doBinAction:flag">`,
		`<button data-action-submit="confirmDeleteForm">`,
	} {
		if m := pageLinkRe.FindStringSubmatch(src); m != nil {
			t.Errorf("data-action false positive: %q captured %q", src, m[1])
		}
	}
}

// TestPageLinks_EveryLinkTheFrontEndNamesHasARoute walks Core's static/ and
// templates/ sources and asks the REAL router (realRouterFor, the production
// NewRouter) whether every captured target is a route. Pinned red by the
// planted-href probe described in the report: a template href nobody serves
// fails here, not in front of a user.
func TestPageLinks_EveryLinkTheFrontEndNamesHasARoute(t *testing.T) {
	h, _ := testHandlersForPages(t)
	handler := realRouterFor(t, h)
	router, ok := handler.(*chi.Mux)
	if !ok {
		t.Fatalf("NewRouter returned %T, not *chi.Mux — Match cannot be asked", handler)
	}

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
	// A Go template beyond this point is conditional markup as often as it is
	// a path segment ("/demand-episodes/{{.OriginID}}"), so the literal
	// prefix is the only part that can be checked without guessing which.
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

// routeExists asks whether any registered route answers this path, for
// either a link (GET) or a form action (POST) — Core's templates post to
// /config/save, /payloads/create and friends, which a GET-only question
// would flag. A captured prefix is tried with a placeholder segment too:
// concatenation ("/missions/" + orderID) and template truncation both leave
// a prefix whose real route takes a parameter, and neither is a finding.
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
		if router.Match(chi.NewRouteContext(), "POST", c) {
			return true
		}
	}
	return false
}
