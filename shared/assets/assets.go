// Package assets serves a binary's embedded static files: one handler both
// routers mount, built once at start-up from the embed, so a request costs
// the server a map lookup and a write.
//
// WHY ONE HANDLER, BUILT ONCE. Both routers used to send every static file
// through http.FileServer and chi's Compress, so each request gzipped its file
// again — 17.6 ms of CPU per request for composer-model.js, measured on an
// Edge Pi 4 — and the ETag was the process's start time, so every restart
// sent every kiosk the whole page set again. Here each file is read once at
// New: CSS comments are stripped, the served bytes are hashed, and a
// compressible file is gzipped once and held.
//
// WHAT IS HELD, PER FILE: the gzip of the served bytes when that is smaller,
// and otherwise the served bytes only where they differ from the embed. Never
// both. A client that does not accept gzip — no browser — gets the held gzip
// decompressed, or the embed's own bytes, which live in the binary and cost no
// heap.
//
// THE ETAG IS THE CONTENT. It is a hash of the served bytes, so two builds of
// the same file answer the same ETag, and a restart that changed nothing costs
// a kiosk a round of 304s instead of a re-download. The per-process build id
// the SSE stream carries is not this; it still decides when a page reloads.
//
// TWO URL FORMS for one file. The plain one (/static/a/x.js) answers
// Cache-Control: no-cache, so the browser revalidates on every use. The hashed
// one (URL: /static/a/x.<hash>.js) answers immutable for a year. The hash sits
// in the file name, not in a directory or a query: a module's relative imports
// resolve against its directory, so a module loaded by its hashed URL still
// imports its neighbours by their plain URLs, which the page's import map
// (ImportMap) sends to their hashed ones. A query is what the template guard
// (static_asset_query_guard_test.go) forbids.
//
// *.test.js is never served, whatever the embed holds.
package assets

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Mount serves FS's files under Prefix: "/static/shared/" on shared.Files.
type Mount struct {
	Prefix string
	FS     fs.FS
}

// hashLen is the hex length of the hash in an ETag and a hashed URL: 64 bits,
// which no two versions of one file will share.
const hashLen = 16

// gzipLevel is paid once per file at start-up, not per request, so it is the
// smallest output rather than the fastest.
const gzipLevel = gzip.BestCompression

// immutableCacheControl is the hashed URL's: its bytes can never change.
const immutableCacheControl = "public, max-age=31536000, immutable"

// Handler serves every file of its mounts. Build it with New.
type Handler struct {
	files map[string]*file // by plain URL path
	stats Stats
}

type file struct {
	fsys  fs.FS
	name  string // path inside fsys
	ctype string
	hash  string
	etag  string
	size  int64 // length of the served bytes
	// gz is the gzip of the served bytes, held when it is smaller.
	gz []byte
	// plain is the served bytes, held only when they differ from the embed
	// and gz is nil (a stripped stylesheet gzip does not shrink).
	plain []byte
	// sourceServed: the served bytes are the embed's own, so a client
	// without gzip is answered by streaming the embed.
	sourceServed bool
}

// Stats is what New built, for the start-up cost a report or a test reads.
type Stats struct {
	Files       int
	SourceBytes int64 // the embedded files served, as embedded
	ServedBytes int64 // the same after stripping
	HeldBytes   int64 // what the handler keeps on the heap: gzip and plain copies
	Took        time.Duration
}

// Stats reports what New built.
func (h *Handler) Stats() Stats { return h.stats }

// New reads every file of the mounts once and prepares it. Where two mounts'
// prefixes nest, the longer one owns its URLs, as the router's longer route
// would. The work runs on every core: it is the one CPU cost this package
// adds, and it is paid before the router answers.
func New(mounts ...Mount) (*Handler, error) {
	t0 := time.Now()
	type job struct {
		url  string
		fsys fs.FS
		name string
	}
	var jobs []job
	for _, m := range mounts {
		if !strings.HasPrefix(m.Prefix, "/") || !strings.HasSuffix(m.Prefix, "/") {
			return nil, fmt.Errorf("assets: mount prefix %q must begin and end with /", m.Prefix)
		}
		err := fs.WalkDir(m.FS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || strings.HasSuffix(p, ".test.js") {
				return nil
			}
			u := m.Prefix + p
			for _, n := range mounts {
				if len(n.Prefix) > len(m.Prefix) && strings.HasPrefix(u, n.Prefix) {
					return nil // the longer mount's route answers this URL
				}
			}
			jobs = append(jobs, job{url: u, fsys: m.FS, name: p})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("assets: walk %s: %w", m.Prefix, err)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].url < jobs[j].url })

	h := &Handler{files: make(map[string]*file, len(jobs))}
	for _, j := range jobs {
		if _, dup := h.files[j.url]; dup {
			return nil, fmt.Errorf("assets: %s is served by two mounts", j.url)
		}
		h.files[j.url] = nil
	}

	built := make([]*file, len(jobs))
	src := make([]int64, len(jobs))
	errs := make([]error, len(jobs))
	var next atomic.Int64
	var wg sync.WaitGroup
	workers := min(runtime.GOMAXPROCS(0), len(jobs))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzipLevel)
			for {
				i := int(next.Add(1) - 1)
				if i >= len(jobs) {
					return
				}
				j := jobs[i]
				built[i], src[i], errs[i] = buildFile(j.url, j.fsys, j.name, zw, &buf)
			}
		}()
	}
	wg.Wait()
	for i, j := range jobs {
		if errs[i] != nil {
			return nil, errs[i]
		}
		f := built[i]
		h.files[j.url] = f
		h.stats.Files++
		h.stats.SourceBytes += src[i]
		h.stats.ServedBytes += f.size
		h.stats.HeldBytes += int64(len(f.gz) + len(f.plain))
	}
	for u := range h.files {
		// A real file named like another's hashed form would make one URL
		// mean two files.
		if plain, _, ok := splitHashed(u); ok && h.files[plain] != nil {
			return nil, fmt.Errorf("assets: %s is named like the hashed URL of %s", u, plain)
		}
	}
	h.stats.Took = time.Since(t0)
	return h, nil
}

func buildFile(u string, fsys fs.FS, name string, zw *gzip.Writer, buf *bytes.Buffer) (*file, int64, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, 0, fmt.Errorf("assets: read %s: %w", u, err)
	}
	f := &file{fsys: fsys, name: name, ctype: contentType(name, b)}
	served := b
	if path.Ext(name) == ".css" {
		served = StripCSSComments(b)
	}
	sum := sha256.Sum256(served)
	f.hash = hex.EncodeToString(sum[:])[:hashLen]
	f.etag = `"` + f.hash + `"`
	f.size = int64(len(served))
	f.sourceServed = bytes.Equal(served, b)
	if compressible(f.ctype) {
		buf.Reset()
		zw.Reset(buf)
		if _, err := zw.Write(served); err != nil {
			return nil, 0, fmt.Errorf("assets: gzip %s: %w", u, err)
		}
		if err := zw.Close(); err != nil {
			return nil, 0, fmt.Errorf("assets: gzip %s: %w", u, err)
		}
		if buf.Len() < len(served) {
			f.gz = bytes.Clone(buf.Bytes())
		}
	}
	if f.gz == nil && !f.sourceServed {
		f.plain = served
	}
	return f, int64(len(b)), nil
}

// contentTypes is the table for the extensions the trees serve, so a file's
// type is the same on every host. http.FileServer asked mime.TypeByExtension,
// which reads the Windows registry or the Linux mime database: a .ico was
// image/x-icon on one host and image/vnd.microsoft.icon on another, and a
// .js could be text/javascript or application/javascript. .js is RFC 9239's
// text/javascript; .ico is image/x-icon, the type Edge's header.html declares
// on its icon link.
var contentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".html":  "text/html; charset=utf-8",
	".ico":   "image/x-icon",
	".js":    "text/javascript; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".mjs":   "text/javascript; charset=utf-8",
	".png":   "image/png",
	".svg":   "image/svg+xml",
	".txt":   "text/plain; charset=utf-8",
	".woff2": "font/woff2",
}

// contentType is the table's type for name, or, for an extension it does not
// hold, what the bytes sniff as (http.DetectContentType, the same on every
// host). Never the host's mime table.
func contentType(name string, b []byte) string {
	if ct, ok := contentTypes[strings.ToLower(path.Ext(name))]; ok {
		return ct
	}
	return http.DetectContentType(b)
}

// compressible is chi's Compress list (middleware.defaultCompressibleContentTypes),
// which decided what the routers gzipped before this package: the same files
// are gzipped now, once.
func compressible(ct string) bool {
	base, _, _ := strings.Cut(ct, ";")
	switch strings.TrimSpace(base) {
	case "text/html", "text/css", "text/plain", "text/javascript",
		"application/javascript", "application/x-javascript", "application/json",
		"application/atom+xml", "application/rss+xml", "image/svg+xml":
		return true
	}
	return false
}

// ServeHTTP answers the file the request path names, in its plain or its
// hashed form. Mount it on the full path: no StripPrefix.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f, immutable := h.lookup(r.URL.Path)
	if f == nil {
		http.NotFound(w, r)
		return
	}
	serve(w, r, f, immutable)
}

// File answers one asset whatever the request path: /favicon.ico, which a
// browser asks for at the root, is /static/favicon.ico. It panics when the
// asset is not one this handler serves, since that is a wiring mistake the
// first test of the router finds.
func (h *Handler) File(urlPath string) http.Handler {
	f := h.files[urlPath]
	if f == nil {
		panic("assets: File(" + urlPath + "): no such asset")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serve(w, r, f, false) })
}

func (h *Handler) lookup(p string) (*file, bool) {
	if f := h.files[p]; f != nil {
		return f, false
	}
	plain, hash, ok := splitHashed(p)
	if !ok {
		return nil, false
	}
	f := h.files[plain]
	if f == nil {
		return nil, false
	}
	// A stale hash — a page rendered before a restart asking for a file that
	// has changed since — is answered the current file under no-cache rather
	// than redirected: the redirect would cost a second round trip on the
	// plant Wi-Fi to fetch the same bytes, and no-cache keeps the stale URL
	// from holding them for a year. The build-change reload brings the page
	// onto the new URLs.
	return f, hash == f.hash
}

func serve(w http.ResponseWriter, r *http.Request, f *file, immutable bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	hdr := w.Header()
	hdr.Set("ETag", f.etag)
	if immutable {
		hdr.Set("Cache-Control", immutableCacheControl)
	} else {
		hdr.Set("Cache-Control", "no-cache")
	}
	if f.gz != nil {
		hdr.Set("Vary", "Accept-Encoding")
	}
	if etagMatch(r.Header.Get("If-None-Match"), f.etag) {
		// No Content-Type, no length, no body: a 304 describes nothing new.
		w.WriteHeader(http.StatusNotModified)
		return
	}
	hdr.Set("Content-Type", f.ctype)
	if f.gz != nil && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		hdr.Set("Content-Encoding", "gzip")
		hdr.Set("Content-Length", strconv.Itoa(len(f.gz)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(f.gz)
		}
		return
	}
	hdr.Set("Content-Length", strconv.FormatInt(f.size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	switch {
	case f.plain != nil:
		_, _ = w.Write(f.plain)
	case f.sourceServed:
		src, err := f.fsys.Open(f.name)
		if err != nil {
			return // the embed was read at New; it cannot be missing now
		}
		defer src.Close()
		_, _ = io.Copy(w, src)
	default:
		zr, err := gzip.NewReader(bytes.NewReader(f.gz))
		if err != nil {
			return // written by New; cannot be corrupt
		}
		_, _ = io.Copy(w, zr)
	}
}

// etagMatch is If-None-Match's weak comparison over its list.
func etagMatch(inm, etag string) bool {
	if inm == "" {
		return false
	}
	for _, part := range strings.Split(inm, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == etag {
			return true
		}
	}
	return false
}

// acceptsGzip reads Accept-Encoding: gzip at a non-zero q, or * when gzip is
// not named.
func acceptsGzip(ae string) bool {
	star := false
	for _, part := range strings.Split(ae, ",") {
		name, params, _ := strings.Cut(part, ";")
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(k, "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "gzip", "x-gzip":
			return q > 0
		case "*":
			star = q > 0
		}
	}
	return star
}

// URL is the hashed form of an asset's plain URL path, for a template's tag:
// /static/a/x.js → /static/a/x.<hash>.js. A path the handler does not serve
// comes back unchanged, so a typo in a template is a 404 on the page's own
// URL rather than a render failure.
func (h *Handler) URL(p string) string {
	f := h.files[p]
	if f == nil {
		return p
	}
	return hashedURL(p, f.hash)
}

func hashedURL(p, hash string) string {
	dir, base := path.Split(p)
	ext := path.Ext(base)
	if ext == "" || ext == base {
		return dir + base + "." + hash
	}
	return dir + strings.TrimSuffix(base, ext) + "." + hash + ext
}

// splitHashed undoes hashedURL: the plain path and the hash, when p's last
// segment carries one.
func splitHashed(p string) (plain, hash string, ok bool) {
	dir, base := path.Split(p)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if i := len(stem) - hashLen - 1; i > 0 && stem[i] == '.' && isHash(stem[i+1:]) {
		return dir + stem[:i] + ext, stem[i+1:], true
	}
	if len(ext) == hashLen+1 && stem != "" && isHash(ext[1:]) {
		return dir + stem, ext[1:], true
	}
	return "", "", false
}

func isHash(s string) bool {
	if len(s) != hashLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
