// Package assetcheck holds a page's static set to what the asset handler
// promises, for the gates both binaries' www tests run on every page: every
// module a page reaches is in its import map, a browser without import maps
// still loads every module through its plain ETag URL, one module is one URL
// either way, no *.test.js is reached, and the boot's requests and bytes are
// counted as a browser would make them.
//
// THREE PIECES. Skeleton renders a page template with no data: its text and
// its asset tags, every branch taken, so a page that needs a database to
// render is still checked. Load is the browser: it reads the tags, applies
// the import map or ignores it, fetches every file from the handler and
// follows each module's imports. Templates is the source guard: a static
// asset is named only as {{assetURL "…"}}, a module entry only as an inline
// import.
package assetcheck

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"sort"
	"strings"
	"text/template/parse"

	"shingo/shared/assets"
)

// Skeleton is the page template entry of set rendered with no data: its
// text, every {{assetURL}} and {{importMap}} call evaluated on h, every other
// action left empty, both sides of every if, range and with, and every
// {{template}} followed. It is a superset of any one render's tags, which is
// what a gate over the tags wants.
func Skeleton(set *template.Template, entry string, h *assets.Handler) (string, error) {
	w := &skeleton{set: set, h: h, open: map[string]bool{}}
	if err := w.template(entry); err != nil {
		return "", err
	}
	return w.b.String(), nil
}

type skeleton struct {
	set  *template.Template
	h    *assets.Handler
	b    strings.Builder
	open map[string]bool
}

func (w *skeleton) template(name string) error {
	t := w.set.Lookup(name)
	if t == nil || t.Tree == nil {
		return fmt.Errorf("assetcheck: no template %q", name)
	}
	if w.open[name] {
		return nil
	}
	w.open[name] = true
	defer delete(w.open, name)
	return w.list(t.Tree.Root)
}

func (w *skeleton) list(l *parse.ListNode) error {
	if l == nil {
		return nil
	}
	for _, n := range l.Nodes {
		if err := w.node(n); err != nil {
			return err
		}
	}
	return nil
}

func (w *skeleton) node(n parse.Node) error {
	switch n := n.(type) {
	case *parse.TextNode:
		w.b.Write(n.Text)
	case *parse.ActionNode:
		w.action(n.Pipe)
	case *parse.IfNode:
		return w.branch(&n.BranchNode)
	case *parse.RangeNode:
		return w.branch(&n.BranchNode)
	case *parse.WithNode:
		return w.branch(&n.BranchNode)
	case *parse.TemplateNode:
		return w.template(n.Name)
	case *parse.ListNode:
		return w.list(n)
	}
	return nil
}

func (w *skeleton) branch(b *parse.BranchNode) error {
	if err := w.list(b.List); err != nil {
		return err
	}
	return w.list(b.ElseList)
}

// action writes an assetURL or importMap call's value; any other action is
// the page's data, which a skeleton has none of. The html/template escaper
// appends its own commands to a pipe once a set has run; they are skipped.
func (w *skeleton) action(p *parse.PipeNode) {
	if p == nil || len(p.Cmds) == 0 {
		return
	}
	args := p.Cmds[0].Args
	if len(args) == 0 {
		return
	}
	fn, ok := args[0].(*parse.IdentifierNode)
	if !ok {
		return
	}
	var lits []string
	for _, a := range args[1:] {
		s, ok := a.(*parse.StringNode)
		if !ok {
			return
		}
		lits = append(lits, s.Text)
	}
	switch {
	case fn.Ident == "assetURL" && len(lits) == 1:
		w.b.WriteString(w.h.URL(lits[0]))
	case fn.Ident == "importMap":
		w.b.WriteString(string(w.h.ImportMapTag(lits...)))
	}
}

// Mode is the browser Load plays.
type Mode int

const (
	// WithImportMap is Chromium 89 and later: specifiers go through the map.
	WithImportMap Mode = iota
	// NoImportMap is a browser that ignores <script type="importmap">.
	NoImportMap
)

func (m Mode) String() string {
	if m == NoImportMap {
		return "no import map"
	}
	return "import map"
}

// Fetch is one request a page's load made.
type Fetch struct {
	URL  string
	Kind string // module, script, style, icon
	// Lazy is a module reached only through import(): fetched when the call
	// runs, not at boot.
	Lazy         bool
	Status       int
	Bytes        int64 // the body as sent: the gzip when the handler holds one
	CacheControl string
	ETag         string
}

// Result is a page's load.
type Result struct {
	ImportMap map[string]string // the page's own, as written
	Fetches   []Fetch
	Problems  []string
}

// Boot is the requests and body bytes of a cold load before any import()
// runs.
func (r Result) Boot() (requests int, bytes int64) {
	for _, f := range r.Fetches {
		if !f.Lazy {
			requests++
			bytes += f.Bytes
		}
	}
	return requests, bytes
}

// Modules is the plain URL of every module the load reached, boot and lazy,
// sorted.
func (r Result) Modules() []string {
	var out []string
	for _, f := range r.Fetches {
		if f.Kind == "module" {
			p, _ := assets.PlainURL(f.URL)
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

var (
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	tagRe         = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>|<link\b([^>]*)>`)
	attrRe        = regexp.MustCompile(`(?s)([a-zA-Z_:][-a-zA-Z0-9_:.]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?`)
)

func attrs(s string) map[string]string {
	out := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(s, -1) {
		out[strings.ToLower(m[1])] = html.UnescapeString(m[2] + m[3] + m[4])
	}
	return out
}

// Load loads page's static set from h as a browser in mode would: the
// stylesheets, icons and classic scripts its tags name, and every module its
// inline module scripts import, followed through each module's static
// imports (the boot) and then its import() calls (lazy). Each request asks
// for gzip; each module body is read for its imports as fetched.
func Load(h http.Handler, page string, mode Mode) Result {
	l := &loader{h: h, mode: mode, byURL: map[string]bool{}, byPlain: map[string]string{}}
	var entries []string
	mapSeen, moduleSeen := false, false
	for _, m := range tagRe.FindAllStringSubmatch(htmlCommentRe.ReplaceAllString(page, ""), -1) {
		if strings.HasPrefix(strings.ToLower(m[0]), "<link") {
			a := attrs(m[3])
			rel := " " + strings.ToLower(a["rel"]) + " "
			switch {
			case strings.Contains(rel, " stylesheet "):
				l.asset(a["href"], "style")
			case strings.Contains(rel, " icon "):
				l.asset(a["href"], "icon")
			case strings.Contains(rel, " modulepreload "):
				l.problem("<link rel=modulepreload href=%q>: a preload is not covered by these checks", a["href"])
			}
			continue
		}
		a := attrs(m[1])
		src, hasSrc := a["src"]
		switch typ := strings.ToLower(strings.TrimSpace(a["type"])); typ {
		case "importmap":
			switch {
			case mapSeen:
				l.problem("a second import map: a browser before Chromium 133 ignores it")
				continue
			case moduleSeen:
				l.problem("the import map comes after a module script: a browser before Chromium 133 ignores it")
			}
			mapSeen = true
			var im struct {
				Imports map[string]string `json:"imports"`
			}
			if err := json.Unmarshal([]byte(m[2]), &im); err != nil {
				l.problem("the import map is not JSON: %v", err)
				continue
			}
			l.res.ImportMap = im.Imports
			if mode == WithImportMap {
				l.imap = im.Imports
			}
		case "module":
			moduleSeen = true
			if hasSrc {
				l.problem(`<script type="module" src=%q>: name a module entry as an inline import, `+
					`so it is one URL with the map and without`, src)
				entries = append(entries, src)
				continue
			}
			static, dynamic := assets.ScanModuleImports([]byte(m[2]))
			entries = append(entries, static...)
			if len(dynamic) > 0 {
				l.lazy = append(l.lazy, specAt{"/", dynamic})
			}
		case "", "text/javascript", "application/javascript":
			if hasSrc {
				l.asset(src, "script")
			}
		}
	}
	// Modules run after the document is parsed, so the map they resolve
	// through is the one the whole document carried.
	l.module("/", entries, false)
	for len(l.lazy) > 0 {
		s := l.lazy[0]
		l.lazy = l.lazy[1:]
		l.module(s.base, s.spec, true)
	}
	return l.res
}

type specAt struct {
	base string
	spec []string
}

type loader struct {
	h       http.Handler
	mode    Mode
	imap    map[string]string
	res     Result
	byURL   map[string]bool
	byPlain map[string]string
	lazy    []specAt
}

func (l *loader) problem(format string, args ...any) {
	l.res.Problems = append(l.res.Problems, fmt.Sprintf(format, args...))
}

// asset fetches a stylesheet, icon or classic script. Its tag is the
// template's {{assetURL}}, so its URL is the hashed one, cached for good.
func (l *loader) asset(u, kind string) {
	if !strings.HasPrefix(u, "/") || strings.HasPrefix(u, "//") || l.byURL[u] {
		return
	}
	l.byURL[u] = true
	f, _ := l.fetch(u, kind, false)
	if f.Status != http.StatusOK {
		return
	}
	if _, hashed := assets.PlainURL(u); !hashed {
		l.problem("%s %s is its plain URL: name it {{assetURL %q}}, the hashed form cached for good", kind, u, u)
	} else if !strings.Contains(f.CacheControl, "immutable") {
		l.problem("%s %s carries a stale hash (Cache-Control %q)", kind, u, f.CacheControl)
	}
}

// module loads the module specs name, relative to base, and what each
// imports: statically as part of the same load, by import() as lazy.
func (l *loader) module(base string, specs []string, lazy bool) {
	for _, s := range specs {
		u, ok := l.resolve(base, s)
		if !ok || l.byURL[u] {
			continue
		}
		l.byURL[u] = true
		plain, hashed := assets.PlainURL(u)
		if other, dup := l.byPlain[plain]; dup {
			l.problem("%s is loaded as %s and as %s: two module instances", plain, other, u)
		}
		l.byPlain[plain] = u
		if strings.HasSuffix(plain, ".test.js") {
			l.problem("%s is reached from the page: a test file", plain)
		}
		f, body := l.fetch(u, "module", lazy)
		if f.Status != http.StatusOK {
			continue
		}
		switch {
		case l.mode == WithImportMap && !hashed:
			l.problem("module %s is reached and is not in the page's import map", plain)
		case l.mode == WithImportMap && !strings.Contains(f.CacheControl, "immutable"):
			l.problem("module %s: the import map carries a stale hash", plain)
		case l.mode == NoImportMap && hashed:
			l.problem("module %s is loaded by its hashed URL with no import map: a module that imports it plain "+
				"holds a second instance", u)
		case l.mode == NoImportMap:
			l.revalidate(f)
		}
		static, dynamic := assets.ScanModuleImports(body)
		l.module(u, static, lazy)
		if len(dynamic) > 0 {
			l.lazy = append(l.lazy, specAt{u, dynamic})
		}
	}
}

// revalidate is the plain URL's freshness path: no-cache with an ETag, and
// the ETag answered 304.
func (l *loader) revalidate(f Fetch) {
	if f.ETag == "" || f.CacheControl != "no-cache" {
		l.problem("module %s answers ETag %q, Cache-Control %q: the plain URL must revalidate", f.URL, f.ETag, f.CacheControl)
		return
	}
	req := httptest.NewRequest(http.MethodGet, f.URL, nil)
	req.Header.Set("If-None-Match", f.ETag)
	rec := httptest.NewRecorder()
	l.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		l.problem("module %s with If-None-Match %s answered %d, want 304", f.URL, f.ETag, rec.Code)
	}
}

// resolve is the URL a browser fetches for spec in the module at base: an
// absolute or relative path resolved, then looked up in the import map.
func (l *loader) resolve(base, spec string) (string, bool) {
	var u string
	switch {
	case strings.ContainsAny(spec, "?#"):
		l.problem("import %q carries a query or fragment: the map and the handler know plain paths only", spec)
		return "", false
	case strings.HasPrefix(spec, "/") && !strings.HasPrefix(spec, "//"):
		u = path.Clean(spec)
	case strings.HasPrefix(spec, "./"), strings.HasPrefix(spec, "../"):
		u = path.Join(path.Dir(base), spec)
	default:
		if v, ok := l.imap[spec]; ok {
			return v, true
		}
		l.problem("import %q in %s is neither a path nor in the import map", spec, base)
		return "", false
	}
	if v, ok := l.imap[u]; ok {
		return v, true
	}
	return u, true
}

func (l *loader) fetch(u, kind string, lazy bool) (Fetch, []byte) {
	req := httptest.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	l.h.ServeHTTP(rec, req)
	body := rec.Body.Bytes()
	f := Fetch{
		URL: u, Kind: kind, Lazy: lazy, Status: rec.Code, Bytes: int64(len(body)),
		CacheControl: rec.Header().Get("Cache-Control"), ETag: rec.Header().Get("ETag"),
	}
	l.res.Fetches = append(l.res.Fetches, f)
	if f.Status != http.StatusOK {
		l.problem("%s %s answered %d", kind, u, f.Status)
		return f, nil
	}
	if rec.Header().Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			l.problem("%s %s: gzip: %v", kind, u, err)
			return f, nil
		}
		if body, err = io.ReadAll(zr); err != nil {
			l.problem("%s %s: gzip: %v", kind, u, err)
			return f, nil
		}
	}
	return f, body
}

// srcHrefRe reads every src= and href= value out of template source, an
// action inside the quotes included: src="{{assetURL "/static/x.js"}}".
var (
	srcHrefRe     = regexp.MustCompile(`(?is)\b(src|href)\s*=\s*(?:"((?:\{\{.*?\}\}|[^"])*)"|'((?:\{\{.*?\}\}|[^'])*)')`)
	assetURLRe    = regexp.MustCompile(`^\{\{\s*assetURL\s+"([^"]*)"\s*\}\}$`)
	scriptOpenRe  = regexp.MustCompile(`(?is)<script\b((?:\{\{.*?\}\}|[^>])*)>`)
	inlineModRe   = regexp.MustCompile(`(?is)<script\s+type="module"\s*>(.*?)</script\s*>`)
	importMapRe   = regexp.MustCompile(`\{\{\s*importMap((?:\s+"[^"]*")*)\s*\}\}`)
	importMapArgs = regexp.MustCompile(`"([^"]*)"`)
	tmplComment   = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)
	typeModuleRe  = regexp.MustCompile(`(?i)\btype\s*=\s*["']?module\b`)
	srcAttrRe     = regexp.MustCompile(`(?i)\bsrc\s*=`)
)

// Templates is the source guard over every template the patterns match in
// fsys: a static asset in a src or href is named only as {{assetURL "…"}} on a
// path h serves, a module script has no src, and every importMap argument and
// inline module import is a plain path h serves. An HTML comment is text and
// is read; a template comment is not.
func Templates(fsys fs.FS, h *assets.Handler, patterns ...string) ([]string, error) {
	var files []string
	for _, pat := range patterns {
		m, err := fs.Glob(fsys, pat)
		if err != nil {
			return nil, err
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("assetcheck: no templates match %v", patterns)
	}
	var problems []string
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		src := tmplComment.ReplaceAllStringFunc(string(b), func(c string) string {
			return strings.Repeat("\n", strings.Count(c, "\n")) // keep line numbers
		})
		at := func(i int) string { return fmt.Sprintf("%s:%d", f, strings.Count(src[:i], "\n")+1) }
		served := func(p string) bool { return !strings.ContainsAny(p, "?#") && h.URL(p) != p }
		for _, m := range srcHrefRe.FindAllStringSubmatchIndex(src, -1) {
			val := ""
			switch {
			case m[4] >= 0:
				val = src[m[4]:m[5]]
			case m[6] >= 0:
				val = src[m[6]:m[7]]
			}
			if !strings.Contains(val, "/static/") && !strings.HasPrefix(val, "/favicon") {
				continue
			}
			if am := assetURLRe.FindStringSubmatch(val); am == nil {
				problems = append(problems, fmt.Sprintf(`%s: %s=%q: name a static asset only as {{assetURL "/static/…"}}, `+
					`the handler's hashed form`, at(m[0]), src[m[2]:m[3]], val))
			} else if !served(am[1]) {
				problems = append(problems, fmt.Sprintf("%s: assetURL %q is not a file the handler serves", at(m[0]), am[1]))
			}
		}
		for _, m := range scriptOpenRe.FindAllStringSubmatchIndex(src, -1) {
			a := src[m[2]:m[3]]
			if typeModuleRe.MatchString(a) && srcAttrRe.MatchString(a) {
				problems = append(problems, fmt.Sprintf(`%s: <script type="module" src=…>: write the entry as `+
					`<script type="module">import "/static/…";</script> after the import map`, at(m[0])))
			}
		}
		for _, m := range inlineModRe.FindAllStringSubmatchIndex(src, -1) {
			static, dynamic := assets.ScanModuleImports([]byte(src[m[2]:m[3]]))
			for _, s := range append(static, dynamic...) {
				if !served(s) {
					problems = append(problems, fmt.Sprintf("%s: inline module imports %q, not a plain path the handler serves",
						at(m[0]), s))
				}
			}
		}
		for _, m := range importMapRe.FindAllStringSubmatchIndex(src, -1) {
			for _, a := range importMapArgs.FindAllStringSubmatch(src[m[2]:m[3]], -1) {
				if !served(a[1]) {
					problems = append(problems, fmt.Sprintf("%s: importMap entry %q is not a plain path the handler serves",
						at(m[0]), a[1]))
				}
			}
		}
	}
	return problems, nil
}
