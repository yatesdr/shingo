package assets

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// A stylesheet with comments, long enough that gzip shrinks it.
var testCSS = "/* tokens */\n:root {\n  --a: 1px; /* the unit */\n}\n" + strings.Repeat(".x { color: red; }\n", 40)

func testTree() fstest.MapFS {
	return fstest.MapFS{
		"app.js":             {Data: []byte("import { a } from './lib.js';\n" + strings.Repeat("console.log(a);\n", 40))},
		"lib.js":             {Data: []byte("export const a = 1;\n")},
		"style.css":          {Data: []byte(testCSS)},
		"plain.css":          {Data: []byte(".y{}\n")},
		"favicon.ico":        {Data: []byte("\x00\x00\x01\x00icon bytes")},
		"icon.svg":           {Data: []byte("<svg xmlns='http://www.w3.org/2000/svg'>" + strings.Repeat("<g/>", 50) + "</svg>")},
		"app.test.js":        {Data: []byte("import './app.js';\n")},
		"pages/page.js":      {Data: []byte("import '/static/shared/util.js';\n")},
		"pages/x.test.js":    {Data: []byte("// a test\n")},
		"shared/shadowed.js": {Data: []byte("// served by the longer mount, not this one\n")},
	}
}

func testShared() fstest.MapFS {
	return fstest.MapFS{
		"util.js":      {Data: []byte("export function util() {}\n" + strings.Repeat("// pad\n", 40))},
		"util.test.js": {Data: []byte("import './util.js';\n")},
	}
}

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	h, err := New(Mount{"/static/", testTree()}, Mount{"/static/shared/", testShared()})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h http.Handler, method, url string, hdr ...string) *http.Response {
	req := httptest.NewRequest(method, url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return b
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func served(t *testing.T, name string) []byte {
	t.Helper()
	src := testTree()[name].Data
	if strings.HasSuffix(name, ".css") {
		return StripCSSComments(src)
	}
	return src
}

// The ETag is a function of the served bytes: two handlers built at
// different instants over the same files answer the same ETag, and it is the
// served bytes' sha256. The per-process ETag this replaces cost every kiosk a
// full re-download on every restart.
func TestETagIsTheContentHash(t *testing.T) {
	a := newTestHandler(t)
	time.Sleep(2 * time.Millisecond)
	b := newTestHandler(t)
	for _, name := range []string{"app.js", "style.css", "favicon.ico", "icon.svg"} {
		ea := get(a, "GET", "/static/"+name).Header.Get("ETag")
		eb := get(b, "GET", "/static/"+name).Header.Get("ETag")
		sum := sha256.Sum256(served(t, name))
		want := `"` + hex.EncodeToString(sum[:])[:hashLen] + `"`
		if ea != want || eb != want {
			t.Errorf("%s: ETags %s and %s, want %s from the served bytes", name, ea, eb, want)
		}
	}
}

// A 304 carries no body and no Content-Type (chi's Compress, before this,
// would open a gzip stream for any response with a compressible type), but
// keeps the validators and the caching headers a 200 would have sent.
func TestNotModifiedCarriesNoBodyOrType(t *testing.T) {
	h := newTestHandler(t)
	for _, url := range []string{"/static/app.js", "/static/style.css", "/static/favicon.ico"} {
		etag := get(h, "GET", url).Header.Get("ETag")
		for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
			for _, ae := range []string{"", "gzip"} {
				resp := get(h, "GET", url, "If-None-Match", inm, "Accept-Encoding", ae)
				body := readBody(t, resp)
				if resp.StatusCode != http.StatusNotModified {
					t.Fatalf("%s If-None-Match %s: %d, want 304", url, inm, resp.StatusCode)
				}
				if len(body) != 0 || resp.Header.Get("Content-Type") != "" ||
					resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Content-Length") != "" {
					t.Errorf("%s 304: body %d bytes, headers %v", url, len(body), resp.Header)
				}
				if resp.Header.Get("ETag") != etag || resp.Header.Get("Cache-Control") != "no-cache" {
					t.Errorf("%s 304 headers %v", url, resp.Header)
				}
			}
		}
	}
	resp := get(h, "GET", "/static/app.js", "If-None-Match", `"0000000000000000"`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a stale ETag answered %d, want 200", resp.StatusCode)
	}
}

// The gzip body decodes to the served bytes, and a client without gzip gets
// the same bytes: from the embed where they are the embed's, from the held
// gzip where they are not (a stripped stylesheet).
func TestGzipAndPlainBodiesAreTheServedBytes(t *testing.T) {
	h := newTestHandler(t)
	for _, name := range []string{"app.js", "style.css", "icon.svg", "plain.css", "favicon.ico"} {
		want := served(t, name)
		url := "/static/" + name

		z := get(h, "GET", url, "Accept-Encoding", "gzip, deflate")
		zb := readBody(t, z)
		if z.Header.Get("Content-Encoding") == "gzip" {
			if got := gunzip(t, zb); !bytes.Equal(got, want) {
				t.Errorf("%s: gzip body decodes to %q, want the served bytes", name, got)
			}
			if z.Header.Get("Vary") != "Accept-Encoding" || z.Header.Get("Content-Length") != itoa(len(zb)) {
				t.Errorf("%s gzip headers %v", name, z.Header)
			}
		} else if !bytes.Equal(zb, want) {
			t.Errorf("%s: identity body to a gzip client is not the served bytes", name)
		}

		for _, ae := range []string{"", "identity", "gzip;q=0", "gzip;q=0, *"} {
			p := get(h, "GET", url, "Accept-Encoding", ae)
			pb := readBody(t, p)
			if p.Header.Get("Content-Encoding") != "" || !bytes.Equal(pb, want) {
				t.Errorf("%s Accept-Encoding %q: encoding %q, body %q", name, ae, p.Header.Get("Content-Encoding"), pb)
			}
			if p.Header.Get("Content-Length") != itoa(len(want)) {
				t.Errorf("%s Accept-Encoding %q: Content-Length %s, want %d", name, ae, p.Header.Get("Content-Length"), len(want))
			}
		}
	}
	if z := get(h, "GET", "/static/app.js", "Accept-Encoding", "*"); z.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("Accept-Encoding * was answered %q, want gzip", z.Header.Get("Content-Encoding"))
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// What the handler keeps on the heap: one form per file, never a gzip and a
// raw copy both. A file served as embedded holds only its gzip, or nothing.
func TestOneHeldCopyPerFile(t *testing.T) {
	h := newTestHandler(t)
	for u, f := range h.files {
		if f.gz != nil && f.plain != nil {
			t.Errorf("%s holds both a gzip and a raw copy", u)
		}
		if f.sourceServed && f.plain != nil {
			t.Errorf("%s holds a raw copy of bytes the embed already has", u)
		}
	}
	if f := h.files["/static/app.js"]; f.gz == nil || !f.sourceServed {
		t.Errorf("app.js: gz held %v, served as embedded %v; want both", f.gz != nil, f.sourceServed)
	}
	if f := h.files["/static/style.css"]; f.gz == nil || f.sourceServed || f.plain != nil {
		t.Errorf("style.css: want the gzip of the stripped bytes alone")
	}
	if f := h.files["/static/favicon.ico"]; f.gz != nil || f.plain != nil {
		t.Errorf("favicon.ico: not compressible and served as embedded, want nothing held")
	}
	if h.Stats().Files != len(h.files) || h.Stats().HeldBytes == 0 {
		t.Errorf("stats %+v", h.Stats())
	}
}

// No *.test.js is servable, even when the embed holds one, in any form;
// neither is a directory or a file a longer mount owns.
func TestRefused(t *testing.T) {
	h := newTestHandler(t)
	for _, url := range []string{
		"/static/app.test.js", "/static/pages/x.test.js", "/static/shared/util.test.js",
		"/static/app.test.0123456789abcdef.js",
		"/static/", "/static/pages", "/static/pages/",
		"/static/shared/shadowed.js",
		"/static/missing.js", "/static/missing.0123456789abcdef.js",
	} {
		resp := get(h, "GET", url)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusNotFound || string(body) != "404 page not found\n" {
			t.Errorf("%s: %d %q, want 404", url, resp.StatusCode, body)
		}
	}
	if resp := get(h, "POST", "/static/app.js"); resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD" {
		t.Errorf("POST: %d Allow %q, want 405 GET, HEAD", resp.StatusCode, resp.Header.Get("Allow"))
	}
}

// HEAD answers GET's headers and no body.
func TestHead(t *testing.T) {
	h := newTestHandler(t)
	for _, ae := range []string{"", "gzip"} {
		g := get(h, "GET", "/static/app.js", "Accept-Encoding", ae)
		gb := readBody(t, g)
		hd := get(h, "HEAD", "/static/app.js", "Accept-Encoding", ae)
		if b := readBody(t, hd); len(b) != 0 {
			t.Errorf("HEAD carried %d bytes", len(b))
		}
		for _, k := range []string{"Content-Type", "Content-Encoding", "Content-Length", "ETag", "Cache-Control", "Vary"} {
			if g.Header.Get(k) != hd.Header.Get(k) {
				t.Errorf("Accept-Encoding %q: HEAD %s %q, GET %q", ae, k, hd.Header.Get(k), g.Header.Get(k))
			}
		}
		if g.Header.Get("Content-Length") != itoa(len(gb)) {
			t.Errorf("GET Content-Length %s for %d bytes", g.Header.Get("Content-Length"), len(gb))
		}
	}
}

// The hashed URL answers the file immutable; a stale hash answers the
// current file under no-cache (lookup says why); both carry the file's ETag.
// The plain URL stays no-cache.
func TestHashedURL(t *testing.T) {
	h := newTestHandler(t)
	for _, plain := range []string{"/static/app.js", "/static/style.css", "/static/favicon.ico", "/static/shared/util.js", "/static/pages/page.js"} {
		hashed := h.URL(plain)
		if hashed == plain || strings.Contains(hashed, "?") {
			t.Fatalf("URL(%s) = %s", plain, hashed)
		}
		dir := plain[:strings.LastIndex(plain, "/")+1]
		if !strings.HasPrefix(hashed, dir) || strings.Contains(hashed[len(dir):], "/") {
			t.Errorf("URL(%s) = %s moved the file out of its directory: its relative imports would resolve elsewhere", plain, hashed)
		}
		p := get(h, "GET", plain, "Accept-Encoding", "gzip")
		pb := readBody(t, p)
		hr := get(h, "GET", hashed, "Accept-Encoding", "gzip")
		hb := readBody(t, hr)
		if hr.StatusCode != 200 || !bytes.Equal(hb, pb) || hr.Header.Get("ETag") != p.Header.Get("ETag") {
			t.Errorf("%s: %d, same bytes %v, ETag %s vs %s", hashed, hr.StatusCode, bytes.Equal(hb, pb), hr.Header.Get("ETag"), p.Header.Get("ETag"))
		}
		if cc := hr.Header.Get("Cache-Control"); cc != immutableCacheControl {
			t.Errorf("%s Cache-Control %q, want %q", hashed, cc, immutableCacheControl)
		}
		if cc := p.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s Cache-Control %q, want no-cache", plain, cc)
		}
		nm := get(h, "GET", hashed, "If-None-Match", p.Header.Get("ETag"))
		if nm.StatusCode != 304 || nm.Header.Get("Cache-Control") != immutableCacheControl {
			t.Errorf("%s revalidated: %d %q", hashed, nm.StatusCode, nm.Header.Get("Cache-Control"))
		}

		f := h.files[plain]
		stale := hashedURL(plain, strings.Repeat("0", hashLen))
		s := get(h, "GET", stale, "Accept-Encoding", "gzip")
		sb := readBody(t, s)
		if s.StatusCode != 200 || !bytes.Equal(sb, pb) || s.Header.Get("Cache-Control") != "no-cache" || s.Header.Get("ETag") != f.etag {
			t.Errorf("%s (stale): %d, current bytes %v, Cache-Control %q", stale, s.StatusCode, bytes.Equal(sb, pb), s.Header.Get("Cache-Control"))
		}
	}
	if got := h.URL("/static/nope.js"); got != "/static/nope.js" {
		t.Errorf("URL of an unknown path = %s, want it unchanged", got)
	}
}

func TestHashedFormRoundTrips(t *testing.T) {
	const hash = "0123456789abcdef"
	for _, p := range []string{"/static/a/x.js", "/static/a.b/x.min.js", "/static/LICENSE", "/static/.hidden", "/x.css"} {
		hp := hashedURL(p, hash)
		plain, got, ok := splitHashed(hp)
		if !ok || plain != p || got != hash {
			t.Errorf("%s → %s → %s %s %v", p, hp, plain, got, ok)
		}
	}
	for _, p := range []string{"/static/utils.dashboard.test.js", "/static/x.0123456789ABCDEF.js", "/static/x.0123.js", "/static/x.js"} {
		if plain, _, ok := splitHashed(p); ok {
			t.Errorf("%s read as hashed (%s)", p, plain)
		}
	}
}

func TestNewRefusesAmbiguousNames(t *testing.T) {
	tree := fstest.MapFS{
		"x.js":                  {Data: []byte("1")},
		"x.0123456789abcdef.js": {Data: []byte("2")},
	}
	if _, err := New(Mount{"/static/", tree}); err == nil {
		t.Error("a file named like another's hashed URL was accepted")
	}
	if _, err := New(Mount{"static", tree}); err == nil {
		t.Error("a prefix without slashes was accepted")
	}
}

// File serves one asset at any path, as the plain URL does.
func TestFile(t *testing.T) {
	h := newTestHandler(t)
	fav := h.File("/static/favicon.ico")
	a := get(fav, "GET", "/favicon.ico")
	b := get(h, "GET", "/static/favicon.ico")
	if a.StatusCode != 200 || a.Header.Get("ETag") != b.Header.Get("ETag") || a.Header.Get("Content-Type") != "image/x-icon" ||
		!bytes.Equal(readBody(t, a), readBody(t, b)) {
		t.Errorf("/favicon.ico %d %v", a.StatusCode, a.Header)
	}
	defer func() {
		if recover() == nil {
			t.Error("File of an unknown asset did not panic")
		}
	}()
	h.File("/static/nope.ico")
}

// A file's Content-Type is the package's table, or for an extension the table
// does not hold, the sniff of its bytes: never the host's mime table, which
// answers .ico and .js differently on Windows and on the Pi.
func TestContentTypeIsTheTable(t *testing.T) {
	h := newTestHandler(t)
	for url, want := range map[string]string{
		"/static/app.js":         "text/javascript; charset=utf-8",
		"/static/style.css":      "text/css; charset=utf-8",
		"/static/favicon.ico":    "image/x-icon",
		"/static/icon.svg":       "image/svg+xml",
		"/static/shared/util.js": "text/javascript; charset=utf-8",
	} {
		if got := get(h, "GET", url).Header.Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type %q, want %q", url, got, want)
		}
	}
	if got := contentType("notes.unknownext", []byte("plain words\n")); got != "text/plain; charset=utf-8" {
		t.Errorf("an unknown extension: %q, want the sniff of its bytes", got)
	}
}
