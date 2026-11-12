package www

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingo/shared"
	"shingo/shared/assets"
	"shingoedge/internal/testdb"
)

var updateStaticServingGolden = flag.Bool("update-static-serving-golden", false,
	"rewrite testdata/static-serving/edge.txt from what the router's static routes answer")

// TestStaticServingPins characterises what the Edge's static routes answer —
// status, every header but Date, and what the body is relative to the
// embedded source — for a representative set: a JS module, a CSS file, a
// shared file of each kind, both favicon paths, an SVG, a missing file, a
// *.test.js, a directory, and the hashed and a stale-hashed URL of a module
// and a shared stylesheet (probeURL). Each is asked with and without
// Accept-Encoding: gzip, and again with If-None-Match carrying the ETag the
// plain answer gave (the 304 path).
//
// THE ROUTER IS THE REAL ONE (NewRouter through realFlowRouter), so the chain
// pinned is the production chain: the static handler as registerStatic wires
// it, outside chi's Compress.
//
// BODIES ARE DESCRIBED, NOT COPIED. "source" means byte-for-byte the embedded
// file; "source, CSS comments stripped" is what assets.StripCSSComments makes
// of it; "gzip of …" means one gzip layer over that. Sizes are left out on
// purpose: a golden that moved every time someone edited operator.js would pin
// nothing about serving. So is the content-hash ETag: it is written
// "{hash of the served bytes}" when it is that hash (contentETag), and
// {serverInstance} stands for the per-process id wherever an ETag carries it.
// A Content-Type is written as served: the handler's own table, the same on
// every host, so a value read from a host's mime database would fail here on
// the other host.
//
// TestStaticServing_SharedFilesAnswerAlikeOnCore holds the /static/shared/
// answers here to Core's golden.
//
// Regenerate with
//
//	go test ./www -run TestStaticServingPins -update-static-serving-golden
//
// and read the diff.
func TestStaticServingPins(t *testing.T) {
	_, router := realFlowRouter(t, testdb.Open(t))
	got := staticServingPins(t, router, edgeStaticSource, []string{
		"/static/operator-station/operator.js",
		"/static/operator-station/operator.css",
		"/static/shared/dom.js",
		"/static/shared/tokens.css",
		"/favicon.ico",
		"/static/favicon.ico",
		"/static/favicon.svg",
		"/static/operator-station/missing.js",
		"/static/shared/missing.css",
		"/static/js/pages/catid-health.test.js",
		"/static/shared/utils.escape.test.js",
		"/static/operator-station/",
		"{assetURL}/static/operator-station/operator.js",
		"{stale}/static/operator-station/operator.js",
		"{assetURL}/static/shared/tokens.css",
		"{stale}/static/shared/tokens.css",
	})
	checkStaticServingGolden(t, filepath.Join("testdata", "static-serving"), "edge.txt", got)
}

// TestStaticServing_SharedFilesAnswerAlikeOnCore: every /static/shared/
// answer in this golden is the same answer, line for line, in Core's
// (shingo-core/www/testdata/static-serving/core.txt). The two routers mount
// one handler type over one shared.Files, and each golden is held to its own
// router by TestStaticServingPins, so a shared file that answered one way on
// Core and another on Edge — a header, an encoding, a 304, an ETag that is
// not the same hash of the same bytes — fails here.
func TestStaticServing_SharedFilesAnswerAlikeOnCore(t *testing.T) {
	t.Parallel()
	read := func(p string) map[string]string {
		b, err := os.ReadFile(p)
		testutil.MustNoErr(t, err, "read "+p)
		out := map[string]string{}
		for _, block := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n\n") {
			head, _, _ := strings.Cut(block, "\n")
			if f := strings.Fields(head); len(f) > 1 && strings.Contains(f[1], "/static/shared/") {
				out[head] = block
			}
		}
		return out
	}
	edge := read(filepath.Join("testdata", "static-serving", "edge.txt"))
	core := read(filepath.Join("..", "..", "shingo-core", "www", "testdata", "static-serving", "core.txt"))
	if len(edge) == 0 {
		t.Fatal("the Edge golden asks no /static/shared/ path")
	}
	for head, e := range edge {
		c, ok := core[head]
		switch {
		case !ok:
			t.Errorf("Core's golden does not ask %q", head)
		case c != e:
			t.Errorf("%s answers differently:\n  Edge:\n%s\n  Core:\n%s", head, e, c)
		}
	}
	for head := range core {
		if _, ok := edge[head]; !ok {
			t.Errorf("Edge's golden does not ask %q", head)
		}
	}
}

// edgeStaticSource is the embedded file a static URL names, as router.go
// mounts them: /static/shared/ on shared.Files, /static/ and /favicon.ico on
// StaticFS.
func edgeStaticSource(urlPath string) ([]byte, bool) {
	var fsys fs.FS
	var name string
	switch {
	case urlPath == "/favicon.ico":
		fsys, name = StaticFS(), "favicon.ico"
	case strings.HasPrefix(urlPath, "/static/shared/"):
		fsys, name = shared.Files, strings.TrimPrefix(urlPath, "/static/shared/")
	case strings.HasPrefix(urlPath, "/static/"):
		fsys, name = StaticFS(), strings.TrimPrefix(urlPath, "/static/")
	default:
		return nil, false
	}
	b, err := fs.ReadFile(fsys, name)
	return b, err == nil
}

// staticServingPins asks every path the four ways and describes each answer.
// A path may be a probe (probeURL): the hashed or a stale-hashed URL of a
// file, written by the plain path it stands for so the golden does not move
// with the file's content.
func staticServingPins(t *testing.T, h http.Handler, source func(string) ([]byte, bool), paths []string) string {
	t.Helper()
	var out strings.Builder
	for _, p := range paths {
		u, src := probeURL(p)
		plain := serveStatic(h, http.MethodGet, u, false, "")
		etag := plain.Header.Get("ETag")
		for _, c := range []struct {
			gzip bool
			cond bool
		}{{false, false}, {true, false}, {false, true}, {true, true}} {
			if c.cond && etag == "" {
				continue
			}
			inm := ""
			if c.cond {
				inm = etag
			}
			rec := serveStatic(h, http.MethodGet, u, c.gzip, inm)
			fmt.Fprintf(&out, "GET %s gzip=%s if-none-match=%s\n", p, yesNo(c.gzip), yesNo(c.cond))
			out.WriteString(describeStaticAnswer(t, rec, src, source))
			out.WriteString("\n")
		}
	}
	return out.String()
}

// probeURL is the URL a pinned path asks and the plain path whose source it
// answers: "{assetURL}/static/x.js" is the handler's hashed URL of
// /static/x.js (assetURL, as a template will call it), "{stale}/static/x.js"
// the same name with a hash no build gave, as a page rendered before a
// restart would still ask. Any other path is itself.
func probeURL(p string) (u, src string) {
	if plain, ok := strings.CutPrefix(p, "{assetURL}"); ok {
		return staticAssets().URL(plain), plain
	}
	if plain, ok := strings.CutPrefix(p, "{stale}"); ok {
		ext := path.Ext(plain)
		return strings.TrimSuffix(plain, ext) + ".0000000000000000" + ext, plain
	}
	return p, p
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func serveStatic(h http.Handler, method, p string, gz bool, inm string) *http.Response {
	req := httptest.NewRequest(method, p, nil)
	if gz {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	if inm != "" {
		req.Header.Set("If-None-Match", inm)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func describeStaticAnswer(t *testing.T, resp *http.Response, urlPath string, source func(string) ([]byte, bool)) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	testutil.MustNoErr(t, err, "read "+urlPath)
	resp.Body.Close()
	src, haveSrc := source(urlPath)

	var b strings.Builder
	fmt.Fprintf(&b, "  status %d\n", resp.StatusCode)
	keys := make([]string, 0, len(resp.Header))
	for k := range resp.Header {
		if k == "Date" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range resp.Header.Values(k) {
			fmt.Fprintf(&b, "  %s: %s\n", k, normalizeStaticHeader(k, v, urlPath, body, src))
		}
	}
	fmt.Fprintf(&b, "  body: %s\n", describeStaticBody(t, body, src, haveSrc))
	return b.String()
}

func normalizeStaticHeader(k, v, urlPath string, body, src []byte) string {
	switch k {
	case "Etag":
		if v == contentETag(urlPath, src) {
			return `"{hash of the served bytes}"`
		}
		return strings.ReplaceAll(v, serverInstance, "{serverInstance}")
	case "Content-Length":
		switch v {
		case fmt.Sprint(len(body)):
			return "length of the body"
		case fmt.Sprint(len(src)):
			return "length of the source"
		}
	}
	return v
}

// contentETag is the ETag the static handler gives a file: the first 16 hex
// of the sha256 of the bytes it serves, which for a stylesheet are the
// source with its comments stripped.
func contentETag(urlPath string, src []byte) string {
	sum := sha256.Sum256(servedBytes(urlPath, src))
	return `"` + hex.EncodeToString(sum[:])[:16] + `"`
}

func servedBytes(urlPath string, src []byte) []byte {
	if path.Ext(urlPath) == ".css" {
		return assets.StripCSSComments(src)
	}
	return src
}

func describeStaticBody(t *testing.T, body, src []byte, haveSrc bool) string {
	t.Helper()
	switch {
	case len(body) == 0:
		return "empty"
	case haveSrc && bytes.Equal(body, src):
		return "source"
	case haveSrc && bytes.Equal(body, assets.StripCSSComments(src)):
		return "source, CSS comments stripped"
	}
	if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
		dec, err := io.ReadAll(zr)
		testutil.MustNoErr(t, err, "gunzip")
		return "gzip of " + describeStaticBody(t, dec, src, haveSrc)
	}
	if bytes.HasPrefix(body, []byte("<!doctype html>")) && bytes.Contains(body, []byte("<pre>")) {
		return fmt.Sprintf("directory listing, *.test.js listed: %s", yesNo(bytes.Contains(body, []byte(".test.js"))))
	}
	if len(body) < 200 {
		return fmt.Sprintf("%q", body)
	}
	return fmt.Sprintf("%d bytes, not the source", len(body))
}

// checkStaticServingGolden compares got with dir/name, or writes it under
// -update-static-serving-golden. The directory is this pin's alone.
func checkStaticServingGolden(t *testing.T, dir, name, got string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if *updateStaticServingGolden {
		testutil.MustNoErr(t, os.MkdirAll(dir, 0o755), "mkdir "+dir)
		testutil.MustNoErr(t, os.WriteFile(p, []byte(got), 0o644), "write "+p)
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: no golden (%v); regenerate with -update-static-serving-golden and read it", p, err)
	}
	if string(want) == got {
		return
	}
	wl, gl := strings.Split(string(want), "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			t.Fatalf("%s moved at line %d; regenerate only for a named change\n  golden: %s\n  served: %s", p, i+1, w, g)
		}
	}
}
