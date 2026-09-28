//go:build docker

package www

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// static_assets_served_test.go — every /static asset a served Core page names
// is an asset the Core server actually serves, compressed. The twin of
// shingo-edge/www/static_assets_served_test.go, ported 2026-09-26 ("one fact,
// one owner" stream). Needs no browser: httptest plus http.DefaultTransport
// against the real router (NewRouter, not a hand-bound handler) and the real
// embedded templates.
//
// TWO BURNS BEHIND IT (both Edge's, and both apply here verbatim — Core's
// layout.html pins the same tag shapes):
//
//  1. A <script src> that 404s is silent. The tag renders, the page renders,
//     and the only trace is the first call into the global that script was
//     meant to set — which surfaces as a throw in some unrelated state, a long
//     way from its cause. That is how Edge's desktop-bodies.js went
//     undetected (2026-09-13). Core's layout pins /static/app.js,
//     /static/theme.js and /static/shared/*.css on every page.
//  2. chi's Compress decides on Content-Type, and a route that leaves the
//     header unset ships every byte uncompressed with nothing said. Core's
//     www crosses the plant network to a shop-floor display just as Edge's
//     does to a Pi.
//
// /static/shared/* is deliberately in scope: it is served from shingo/shared's
// embedded FS (router.go:132), and a stylesheet renamed there without a
// layout.html update is exactly the silent-404 above.

// assetRefRe reads the /static tags out of a served page.
var assetRefRe = regexp.MustCompile(`(?:src|href)="(/static/[^"]+)"`)

func TestStaticAssets_EveryTagThePagesNameIsServedAndCompressed(t *testing.T) {
	h, _ := testHandlersForPages(t)
	router := realRouterFor(t, h)

	// THE ADMIN PAGES ARE GATED. NewRouter seeds admin/admin on a fresh DB
	// (ensureDefaultAdmin, router.go:115), so the same login round trip the
	// Edge twin performs works here: take the session cookie from a real
	// POST /login against this same router, then carry it on every request so
	// /diagnostics renders under the real requireAuth middleware instead of
	// answering the login page — whose asset list is a different and much
	// shorter one.
	var adminCookie *http.Cookie
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if adminCookie != nil && r.URL.Path != "/login" {
			r.AddCookie(adminCookie)
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	// A client that does NOT follow redirects: handleLogin answers 303 and
	// sets the session on THAT response, so following it reads the last hop
	// and comes back empty.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noFollow.PostForm(srv.URL+"/login", url.Values{
		"username": {"admin"}, "password": {"admin"},
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	for _, c := range resp.Cookies() {
		adminCookie = c
	}
	resp.Body.Close()
	if adminCookie == nil {
		t.Fatal("no session cookie from /login — /diagnostics would serve the login page")
	}

	// One public page and one gated page, so the scan covers both middleware
	// branches: the layout every page shares (favicon, tokens.css, app.js)
	// plus a gated page's own pinning.
	for _, page := range []struct {
		label string
		path  string
	}{
		{"the public /overview page", "/overview"},
		{"the gated /diagnostics page", "/diagnostics"},
	} {
		t.Run(page.label, func(t *testing.T) {
			assertAssetsServed(t, srv.URL, page.label, page.path)
		})
	}
}

func assertAssetsServed(t *testing.T, base, label, path string) {
	t.Helper()

	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("%s: read the page: %v", label, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: the page itself answered %d", label, resp.StatusCode)
	}

	refs := assetRefRe.FindAllStringSubmatch(string(body), -1)
	if len(refs) == 0 {
		t.Fatalf("%s: the page names no /static asset at all — either it did not render or the "+
			"tag pattern has drifted, and this test would pass on both", label)
	}

	seen := map[string]bool{}
	checked := 0
	for _, m := range refs {
		ref := html.UnescapeString(m[1])
		if seen[ref] {
			continue
		}
		seen[ref] = true
		checked++

		// THE SUFFIX TEST IS ON THE PATH, NOT THE REF: layout.html pins
		// cache-busted tags (`…/style.css?v={{cacheBust}}`), so a suffix test
		// on the raw ref never fires. Same burn as Edge's twin.
		refPath := ref
		if i := strings.IndexByte(refPath, '?'); i >= 0 {
			refPath = refPath[:i]
		}

		req, err := http.NewRequest(http.MethodGet, base+ref, nil)
		if err != nil {
			t.Fatalf("%s %s: %v", label, ref, err)
		}
		// ASKED FOR EXPLICITLY so the answer can be read: the default
		// transport transparently decompresses and strips Content-Encoding,
		// so a test that does not ask by hand cannot tell compressed from
		// uncompressed.
		req.Header.Set("Accept-Encoding", "gzip")
		ar, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatalf("%s %s: %v", label, ref, err)
		}
		n, copyErr := io.Copy(io.Discard, ar.Body)
		ar.Body.Close()
		if copyErr != nil {
			// A SHORT READ IS NOT A SMALL FILE: n feeds the 1 kB floor below,
			// so a body that died mid-transfer would read as "below the
			// floor, not a finding" — the one way this test can go green on a
			// broken asset.
			t.Fatalf("%s %s: read the asset: %v", label, ref, copyErr)
		}

		if ar.StatusCode != http.StatusOK || n == 0 {
			t.Errorf("%s: %s answered %d with %d bytes — the tag is on the page and the file is "+
				"not on the server", label, ref, ar.StatusCode, n)
			continue
		}
		// Small files are below the compressor's floor and are not a finding.
		if n > 1024 && (strings.HasSuffix(refPath, ".js") || strings.HasSuffix(refPath, ".css")) &&
			ar.Header.Get("Content-Encoding") != "gzip" {
			t.Errorf("%s: %s came back uncompressed (%d bytes, Content-Type %q) — chi's Compress "+
				"decides on Content-Type, and every shop-floor display fetches this over the plant network",
				label, ref, n, ar.Header.Get("Content-Type"))
		}
	}
	if t.Failed() {
		// NOT "all served and compressed" UNDER A FAILURE: the summary is the
		// line a passing run leaves behind; printed after an error it reads
		// as a second verdict contradicting the first.
		return
	}
	t.Logf("%s: %d assets, all served and compressed", label, checked)
}

var _ = fmt.Sprintf // keep fmt import honest if the page list above changes shape
