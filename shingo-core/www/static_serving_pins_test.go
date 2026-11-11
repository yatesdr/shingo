package www

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"shingo/protocol/testutil"
	"shingo/shared"
)

var updateStaticServingGolden = flag.Bool("update-static-serving-golden", false,
	"rewrite testdata/static-serving/core.txt from what the static routes answer")

// TestStaticServingPins characterises what Core's static routes answer —
// status, every header but Date, and what the body is relative to the
// embedded source — for a representative set: a JS module, a CSS file, a
// shared file of each kind, the favicon, an SVG, a missing file, a *.test.js,
// and a directory. Each is asked with and without Accept-Encoding: gzip, and
// again with If-None-Match carrying the ETag the plain answer gave (the 304
// path). The Edge twin is shingo-edge/www/static_serving_pins_test.go.
//
// THE ROUTES ARE NewRouter's static block, built by coreStaticRouter without
// the engine NewRouter needs (a Postgres): chi's Compress, staticCache and
// http.FileServer as router.go wires them.
//
// BODIES ARE DESCRIBED, NOT COPIED, and host-dependent values are named (the
// serverInstance in the ETag, a Content-Type taken from this host's mime
// table): the Edge twin says why.
//
// Regenerate with
//
//	go test ./www -run TestStaticServingPins -update-static-serving-golden
//
// and read the diff.
func TestStaticServingPins(t *testing.T) {
	got := staticServingPins(t, coreStaticRouter(), coreStaticSource, []string{
		"/static/app.js",
		"/static/style.css",
		"/static/shared/utils.js",
		"/static/shared/tokens.css",
		"/static/favicon.ico",
		"/static/favicon.svg",
		"/static/missing.js",
		"/static/shared/missing.css",
		"/static/app.entersubmits.test.js",
		"/static/shared/utils.escape.test.js",
		"/static/pages/",
	})
	checkStaticServingGolden(t, filepath.Join("testdata", "static-serving"), "core.txt", got)
}

// coreStaticRouter is router.go's static block as NewRouter wires it.
func coreStaticRouter() http.Handler {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(middleware.Compress(5))
		r.Handle("/static/shared/*", http.StripPrefix("/static/shared/",
			staticCache(http.FileServer(http.FS(shared.Files))),
		))
		staticSub, _ := fs.Sub(staticFS, "static")
		r.Handle("/static/*", http.StripPrefix("/static/", staticCache(http.FileServer(http.FS(staticSub)))))
	})
	return r
}

// coreStaticSource is the embedded file a static URL names: /static/shared/
// on shared.Files, /static/ on the static tree.
func coreStaticSource(urlPath string) ([]byte, bool) {
	var fsys fs.FS
	var name string
	switch {
	case strings.HasPrefix(urlPath, "/static/shared/"):
		fsys, name = shared.Files, strings.TrimPrefix(urlPath, "/static/shared/")
	case strings.HasPrefix(urlPath, "/static/"):
		fsys, name = staticFS, "static/"+strings.TrimPrefix(urlPath, "/static/")
	default:
		return nil, false
	}
	b, err := fs.ReadFile(fsys, name)
	return b, err == nil
}

// staticServingPins asks every path the four ways and describes each answer.
func staticServingPins(t *testing.T, h http.Handler, source func(string) ([]byte, bool), paths []string) string {
	t.Helper()
	var out strings.Builder
	for _, p := range paths {
		plain := serveStatic(h, http.MethodGet, p, false, "")
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
			rec := serveStatic(h, http.MethodGet, p, c.gzip, inm)
			fmt.Fprintf(&out, "GET %s gzip=%s if-none-match=%s\n", p, yesNo(c.gzip), yesNo(c.cond))
			out.WriteString(describeStaticAnswer(t, rec, p, source))
			out.WriteString("\n")
		}
	}
	return out.String()
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
		return strings.ReplaceAll(v, serverInstance, "{serverInstance}")
	case "Content-Type":
		ext := path.Ext(urlPath)
		if ext != "" && v == mime.TypeByExtension(ext) {
			return "host mime table (" + ext + ")"
		}
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

func describeStaticBody(t *testing.T, body, src []byte, haveSrc bool) string {
	t.Helper()
	switch {
	case len(body) == 0:
		return "empty"
	case haveSrc && bytes.Equal(body, src):
		return "source"
	}
	if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
		dec, err := io.ReadAll(zr)
		testutil.MustNoErr(t, err, "gunzip")
		if haveSrc && bytes.Equal(dec, src) {
			return "gzip of source"
		}
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
