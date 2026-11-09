package docindex

import (
	"path"
	"regexp"
	"strings"
)

// SourceExt are the extensions that make a one-token code span a path even
// without a slash in it (`shingocore.yaml`, `gate.sh`).
var SourceExt = []string{".go", ".md", ".sql", ".sh", ".yaml", ".yml", ".js", ".html", ".css", ".json", ".txt",
	".svg", ".ps1", ".toml", ".mod"}

// ImportRoots are the Go module paths of the five modules and the directory
// each lives in, so `shingocore/engine` resolves to shingo-core/engine.
// TestImportRoots_MatchGoMod holds them to the go.mod files.
var ImportRoots = map[string]string{
	"shingo/protocol":    "protocol",
	"shingo/shared":      "shared",
	"shingocore":         "shingo-core",
	"shingoedge":         "shingo-edge",
	"shingo/integration": "integration",
}

var (
	// lineCiteRe is a file:line citation: a name with a source extension,
	// then a colon and digits.
	lineCiteRe = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_./-]*\.(?:go|md|sql|sh|yaml|yml|js|html|css|json|txt|svg|ps1|toml|mod):[0-9]+`)
	urlRe      = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z0-9+.-]*://|mailto:)`)
	testPathRe = regexp.MustCompile(`^(?:Test|Fuzz|Benchmark|Example)[A-Z0-9_]`)
	// mimeRe is a media type: `text/plain`, `application/x-node-group`.
	mimeRe = regexp.MustCompile(`^(?:application|audio|font|image|message|model|multipart|text|video)/[A-Za-z0-9][A-Za-z0-9.+-]*$`)
)

// foreign reports whether a slashed token names something outside this
// tree: another repo's module path or a host (`github.com/go-chi/chi/v5`,
// `gopkg.in/yaml.v3`: a dot in the first component, which no tracked
// top-level name but a dot-directory has), or a media type.
func foreign(s string) bool {
	first, _, ok := strings.Cut(s, "/")
	if ok && strings.Contains(first, ".") && !strings.HasPrefix(first, ".") {
		return true
	}
	return mimeRe.MatchString(s)
}

// LineCites returns every file:line citation in a line of text.
func LineCites(line string) []string { return lineCiteRe.FindAllString(line, -1) }

// CitedPath reads an inline code span as a path citation. It returns the path
// to resolve and whether the span cites one at all: one token that holds a
// slash or ends in a source extension, and is a path in this tree rather than
// a URL, an anchor, a glob, a placeholder, an absolute path (a route such as
// `/api/orders`, or the host's `/etc/shingo`), a test's subtest path
// (testcite's), another repo's module path or a media type (foreign). What
// follows a colon (a line, a symbol, a port), a #fragment and a Go package
// pattern's /... are not part of the path.
func CitedPath(span string) (string, bool) {
	s := strings.TrimSpace(span)
	if s == "" || strings.ContainsAny(s, " \t") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") ||
		testPathRe.MatchString(s) || urlRe.MatchString(s) || foreign(s) {
		return "", false
	}
	s, _, _ = strings.Cut(s, ":")
	s = strings.TrimSuffix(s, "/...")
	if !strings.Contains(s, "/") && !hasSourceExt(s) {
		return "", false
	}
	return cleanTarget(s)
}

// SourcePath reads a docgen marker's source as a path to resolve as a cited
// path: what follows a colon (a symbol, `types.go:AllTypes`) is not part of
// it.
func SourcePath(src string) (string, bool) {
	s, _, _ := strings.Cut(strings.TrimSpace(src), ":")
	return cleanTarget(s)
}

// LinkTarget reads a link target as a path to resolve relative to the doc:
// "" and false for a URL, a pure #anchor, a glob or a placeholder.
func LinkTarget(target string) (string, bool) {
	return cleanTarget(strings.TrimSpace(target))
}

// cleanTarget drops a #fragment or ?query and reports whether what is left
// is a path to check.
func cleanTarget(s string) (string, bool) {
	if NotAPath(s) {
		return "", false
	}
	if i := strings.IndexAny(s, "#?"); i >= 0 {
		s = s[:i]
	}
	if s == "" || s == "." || s == "./" {
		return "", false
	}
	return s, true
}

// NotAPath reports whether a cited target is not a path in this tree to
// check: a URL (`scheme://`, `mailto:`), a pure anchor, a glob, or a
// placeholder (`<…>`, `{…}`, `$VAR`, `...`).
func NotAPath(s string) bool {
	return urlRe.MatchString(s) || strings.HasPrefix(s, "#") || strings.ContainsAny(s, "*[]<>{}$…") ||
		strings.Contains(s, "...")
}

func hasSourceExt(s string) bool {
	for _, e := range SourceExt {
		if strings.HasSuffix(s, e) && len(s) > len(e) {
			return true
		}
	}
	return false
}

// Resolver answers whether a cited path names something tracked.
type Resolver struct {
	files  map[string]bool
	dirs   map[string]bool
	byBase map[string][]string // last component -> every tracked file and directory with it
}

// NewResolver indexes the tracked paths, and the directories that hold them.
func NewResolver(tracked []string) *Resolver {
	r := &Resolver{files: map[string]bool{}, dirs: map[string]bool{}, byBase: map[string][]string{}}
	for _, p := range tracked {
		r.files[p] = true
		r.byBase[path.Base(p)] = append(r.byBase[path.Base(p)], p)
		for d := path.Dir(p); d != "." && !r.dirs[d]; d = path.Dir(d) {
			r.dirs[d] = true
			r.byBase[path.Base(d)] = append(r.byBase[path.Base(d)], d)
		}
	}
	return r
}

// Exists reports whether p, from the repo root, is a tracked file or a
// directory holding one.
func (r *Resolver) Exists(p string) bool {
	p = path.Clean(p)
	return r.files[p] || r.dirs[p]
}

// relative joins a target to the doc's directory, "" when it climbs out of
// the repo.
func relative(doc, target string) string {
	if strings.HasPrefix(target, "/") {
		return path.Clean(strings.TrimPrefix(target, "/"))
	}
	j := path.Join(path.Dir(doc), target)
	if j == ".." || strings.HasPrefix(j, "../") {
		return ""
	}
	return j
}

// Link reports whether a link target resolves relative to the doc that
// holds it (a leading / is the repo root, as GitHub reads it).
func (r *Resolver) Link(doc, target string) bool {
	j := relative(doc, target)
	return j != "" && r.Exists(j)
}

// Cited reports whether a path a doc cites in prose resolves, trying in
// order: relative to the doc's directory; from the repo root (a Go import
// path read as its module's directory); module-relative, against each of the
// five module roots; a suffix of a path inside the modules, on a path
// component boundary; and, for a bare name, the basename of any tracked file.
func (r *Resolver) Cited(doc, p string) bool {
	if j := relative(doc, p); j != "" && r.Exists(j) {
		return true
	}
	c := path.Clean(p)
	if strings.HasPrefix(c, "../") || c == ".." {
		return false // it climbs out of the doc's directory and did not resolve there
	}
	if r.Exists(c) || r.Exists(fromImport(c)) {
		return true
	}
	for _, m := range Modules {
		if r.Exists(m + "/" + c) {
			return true
		}
	}
	base := path.Base(c)
	if !strings.Contains(c, "/") {
		for _, q := range r.byBase[base] {
			if r.files[q] {
				return true
			}
		}
		return false
	}
	for _, q := range r.byBase[base] {
		if inModule(q) && strings.HasSuffix(q, "/"+c) {
			return true
		}
	}
	return false
}

// fromImport maps a Go import path to its directory in the repo, or "".
func fromImport(p string) string {
	for imp, dir := range ImportRoots {
		if p == imp {
			return dir
		}
		if strings.HasPrefix(p, imp+"/") {
			return dir + p[len(imp):]
		}
	}
	return ""
}

func inModule(p string) bool {
	top, _, _ := strings.Cut(p, "/")
	for _, m := range Modules {
		if top == m {
			return true
		}
	}
	return false
}

// LinkNames reports whether a link target in doc names changed: the link
// read relative to the doc, as Link reads it.
func LinkNames(doc, target, changed string) bool {
	j := relative(doc, target)
	return j != "" && j == changed
}

// Names reports whether a path a doc cites could name changed, a tracked
// path: the same forms the resolver reads (relative to the doc, from the
// root or an import path, module-relative, a component suffix, a basename),
// matched by name so a path the change deleted still matches.
func Names(doc, cited, changed string) bool {
	if j := relative(doc, cited); j != "" && j == changed {
		return true
	}
	c := path.Clean(cited)
	if c == changed || fromImport(c) == changed || strings.HasSuffix(changed, "/"+c) {
		return true
	}
	return !strings.Contains(c, "/") && path.Base(changed) == c
}
