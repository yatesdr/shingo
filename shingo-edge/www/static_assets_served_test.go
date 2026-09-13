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

	"shingo/shared/scenefixtures"
	"shingoedge/internal/testdb"
)

// static_assets_served_test.go — every /static asset a served page names is an
// asset the server actually serves, compressed.
//
// THIS RAN NOWHERE UNTIL NOW. It was a closure inside composer_shots_test.go,
// which is behind //go:build shots and reachable only through
// scripts/composer-shots.sh by hand, on a machine with Chrome installed. It
// needs no browser: httptest plus http.DefaultTransport against the real
// router and the real templates, which is a gate test's shape exactly. It is
// also the ONLY check in the tree that composer-boot.js's deferred bundle is
// served at all — if S11's deferral breaks, every other test is green and the
// station's CHANGEOVER button opens nothing.
//
// TWO BURNS BEHIND IT.
//
//  1. A <script src> that 404s is silent. The tag renders, the page renders,
//     and the only trace is the first call into the global that script was
//     meant to set — which surfaces as a throw in some unrelated state, a long
//     way from its cause. That is how www/static/js/pages/desktop-bodies.js
//     went undetected (2026-09-13).
//  2. chi's Compress decides on Content-Type, and a route that leaves the
//     header unset ships every byte uncompressed with nothing said. Every
//     station asset crosses plant WiFi to a Pi.

// assetRefRe reads the /static tags out of a served page.
var assetRefRe = regexp.MustCompile(`(?:src|href)="(/static/[^"]+)"`)

// deferredComposerAssets are S11's three: composer-boot.js injects them on the
// first composer open, so they are deliberately NOT tags on the station page
// and a scan of its HTML reports them fine by missing them. Named here so the
// one mechanism that keeps 98 kB off a board's boot is covered by the same
// check as everything else.
var deferredComposerAssets = []string{
	"/static/operator-station/composer.css",
	"/static/operator-station/flowspec-data.js",
	"/static/operator-station/composer-render.js",
}

func TestStaticAssets_EveryTagThePagesNameIsServedAndCompressed(t *testing.T) {
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")
	stationID, ok := seeded.Stations["screen-a4"]
	if !ok {
		t.Fatal("the fixture seeds no operator station")
	}
	_, router := realFlowRouter(t, db)

	// THE ADMIN PAGE IS GATED, so the server carries the session an admin
	// would have — taken from a real /login round trip against this same
	// router, so the page is rendered by the real handler behind the real
	// middleware. Without it /processes answers the login page, whose asset
	// list is a different and much shorter one.
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
	// and comes back empty. First login creates the admin user.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noFollow.PostForm(srv.URL+"/login", url.Values{
		"username": {"assets"}, "password": {"assets"},
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	for _, c := range resp.Cookies() {
		adminCookie = c
	}
	resp.Body.Close()
	if adminCookie == nil {
		t.Fatal("no session cookie from /login — /processes would serve the login page")
	}

	for _, page := range []struct {
		label string
		path  string
		extra []string
	}{
		{"the Processes page", fmt.Sprintf("/processes?process=%d", seeded.ProcessID), nil},
		{"the operator station", fmt.Sprintf("/operator/station/%d", stationID), deferredComposerAssets},
	} {
		t.Run(page.label, func(t *testing.T) {
			assertAssetsServed(t, srv.URL, page.label, page.path, page.extra...)
		})
	}
}

func assertAssetsServed(t *testing.T, base, label, path string, extra ...string) {
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
	for _, e := range extra {
		refs = append(refs, []string{"", e})
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

		// THE SUFFIX TEST IS ON THE PATH, NOT THE REF. Nearly every tag on
		// these pages is cache-busted — `…/processes-desktop.js?v=18d4f3bb…`
		// — so `strings.HasSuffix(ref, ".js")` was false for all but the six
		// unbusted ones, and the gzip half of this test did not run on the
		// assets it was written for. Found by the RED run that removed a
		// Content-Type: the asset came back with none and no
		// Content-Encoding, and the test stayed green.
		refPath := ref
		if i := strings.IndexByte(refPath, '?'); i >= 0 {
			refPath = refPath[:i]
		}

		req, err := http.NewRequest(http.MethodGet, base+ref, nil)
		if err != nil {
			t.Fatalf("%s %s: %v", label, ref, err)
		}
		// ASKED FOR EXPLICITLY so the answer can be read. The default
		// transport adds Accept-Encoding: gzip and then decompresses
		// transparently, which also removes Content-Encoding from the
		// response — so a test that does not ask by hand cannot tell a
		// compressed asset from an uncompressed one.
		req.Header.Set("Accept-Encoding", "gzip")
		ar, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatalf("%s %s: %v", label, ref, err)
		}
		n, copyErr := io.Copy(io.Discard, ar.Body)
		ar.Body.Close()
		if copyErr != nil {
			// A SHORT READ IS NOT A SMALL FILE. n is compared against the
			// compressor's 1 kB floor below, so a body that died mid-transfer
			// would read as "below the floor, not a finding" — the one way
			// this test can go green on a broken asset.
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
				"decides on Content-Type, and every station asset crosses plant WiFi to a Pi",
				label, ref, n, ar.Header.Get("Content-Type"))
		}
	}
	if t.Failed() {
		// NOT "all served and compressed" UNDER A FAILURE. The summary is the
		// line a passing run leaves behind; printed after an error it reads as
		// a second verdict contradicting the first.
		return
	}
	t.Logf("%s: %d assets, all served and compressed", label, checked)
}
