// Command testcite reports Go comments that cite a test which does not exist.
//
// WHY. A comment that names a test is read as "this is pinned". When the test
// was renamed, folded or deleted, the comment is a coverage claim with nothing
// behind it, and a review that leans on it goes looking for a pin that is not
// there. Nothing fails when it happens: deadident reads code-shaped citations
// only, and a test name in a comment is a bare word.
//
// WHAT COUNTS AS A CITATION. In any Go comment under the five modules, a bare
// word Test[A-Z]... and, after a `PIN:` marker, Test, Fuzz or Benchmark
// followed by an upper-case letter. A citation may carry a subtest path
// (`/case/inner` after the name); a trailing `.` or `...` is prose. Not
// citations: a prefix pattern (a name ending in `_`, or followed by `*` or
// `.*`), TestMain, a name after a dot (a selector), and a name the repo
// declares as something other than a test function (a type or a method
// spelled Test...); deadident owns whether such a symbol is live.
//
// GRAVESTONES ARE EXEMPT, by the words touching the name. A comment that says
// a test is gone records history on purpose (docs/ground-rules.md: retire by rewriting,
// and say so). The word just before the name is one of was, were, called,
// "used to be", replaces, replaced, inverts, inverted, ports, previously,
// formerly, deleted, removed, "at base"; or the words just after it are
// "was here", "stood here", "was/were/is deleted", "was removed", "was
// renamed", "is gone", deleted, removed. A wider net (any line holding
// "removed") would hide a real pin that sits in a sentence about a removed bin.
// A PIN: marker names a test by contract, so neither this exemption nor the
// symbol one above applies to it.
//
// WHAT RESOLVES. A top-level name resolves to a func Test... (or Fuzz,
// Benchmark) without a receiver in any _test.go file of the five modules,
// build tags ignored: a comment in one module may cite another module's test.
// After a -run flag the name is a pattern, as go test reads it: it resolves
// when some test's name contains it, unless a `$` anchors it.
// A subtest segment resolves against the names that test can give t.Run,
// read statically from its body, with spaces read as `_` the way go test
// prints them:
//
//   - a string literal first argument, t.Run("case", ...);
//   - a field, t.Run(tc.name, ...): every keyed name: "..." literal in the
//     test's body, or in the package's other _test.go declarations when the
//     table is declared outside the test;
//   - an identifier, t.Run(name, ...): the string keys of a map literal, or
//     the elements of a string-slice literal, in the same places.
//
// A name built at run time (fmt.Sprintf, a concatenation, string(mode)) or a
// positional table row cannot be read, so a citation of one is reported: cite
// the parent, or give the case a literal name.
//
// With -docs the same rules read the in-scope markdown docs instead, and a
// go test -run argument in a doc is judged as the regex it is (docs.go).
//
// Usage: go run ./scripts/testcite [-root DIR] [-docs] [-graves]   (exit 1 on a dangling citation)
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Modules are the five Go modules a citation is read in and resolved against.
var Modules = []string{"protocol", "shared", "shingo-core", "shingo-edge", "integration"}

var (
	// citeRe finds a test name with an optional subtest path. Group 1 is an
	// optional PIN: marker, group 2 the name, group 3 the path.
	citeRe = regexp.MustCompile(`(PIN:\s*)?\b((?:Test|Fuzz|Benchmark)[A-Z][A-Za-z0-9_]*)((?:/[A-Za-z0-9_.#+=-]+)*)`)

	graveBefore = []string{"was", "were", "called", "used to be", "replaces", "replaced", "inverts", "inverted",
		"ports", "previously", "formerly", "deleted", "removed", "at base"}
	graveAfter = []string{"was here", "stood here", "was deleted", "were deleted", "is deleted", "was removed",
		"was renamed", "is gone", "deleted", "removed"}

	skipDirs = map[string]bool{"vendor": true, "testdata": true, "node_modules": true}
)

// Hit is one dangling citation.
type Hit struct {
	File string
	Line int
	Cite string // as written, subtest path included
	Why  string
}

func (h Hit) String() string { return fmt.Sprintf("%s:%d: %s %s", h.File, h.Line, h.Cite, h.Why) }

// literals are the string literals a t.Run argument can draw from.
type literals struct {
	fields map[string][]string // field name -> keyed literal values
	keys   []string            // map-literal string keys and string-slice elements
}

func (l *literals) add(o literals) {
	if l.fields == nil {
		l.fields = map[string][]string{}
	}
	for f, vs := range o.fields {
		l.fields[f] = append(l.fields[f], vs...)
	}
	l.keys = append(l.keys, o.keys...)
}

// runNames is how one test function names its subtests.
type runNames struct {
	direct []string        // literal first arguments
	fields map[string]bool // t.Run(x.F, ...) reads field F
	ident  bool            // t.Run(name, ...) reads an identifier
	own    literals        // the literals in the test's own body
}

// names is every subtest name r can produce, as go test prints it, given the
// literals declared outside any test in the same package.
func (r *runNames) names(outside literals) map[string]bool {
	out := map[string]bool{}
	add := func(vs []string) {
		for _, v := range vs {
			out[strings.ReplaceAll(v, " ", "_")] = true
		}
	}
	add(r.direct)
	for f := range r.fields {
		add(r.own.fields[f])
		add(outside.fields[f])
	}
	if r.ident {
		add(r.own.keys)
		add(outside.keys)
	}
	return out
}

// merge joins two declarations of one test in one package (a docker and a
// non-docker file may both declare it).
func (r *runNames) merge(o *runNames) {
	r.direct = append(r.direct, o.direct...)
	for f := range o.fields {
		r.fields[f] = true
	}
	r.ident = r.ident || o.ident
	r.own.add(o.own)
}

// pkgFacts is what one package directory's _test.go files declare.
type pkgFacts struct {
	tests   map[string]*runNames
	outside literals
}

// comment is one comment group as plain text, with the line each of its
// text lines starts on.
type comment struct {
	text  string
	lines []int // lines[i] is the file line of text line i
}

// lineAt is the file line holding byte offset off of c.text.
func (c comment) lineAt(off int) int {
	return c.lines[strings.Count(c.text[:off], "\n")]
}

// fileFacts is what one Go file contributes.
type fileFacts struct {
	dir      string
	tests    map[string]*runNames
	outside  literals
	declared []string // identifiers declared as something other than a test
	examples []string // Example functions, which a -run pattern also matches
	comments []comment
}

func isTestName(name string) bool {
	return strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Fuzz") || strings.HasPrefix(name, "Benchmark")
}

func strLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// collectLiterals reads the keyed field literals, map keys and string-slice
// elements under n.
func collectLiterals(n ast.Node) literals {
	l := literals{fields: map[string][]string{}}
	ast.Inspect(n, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		_, isMap := cl.Type.(*ast.MapType)
		for _, el := range cl.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				if v, ok := strLit(el); ok {
					l.keys = append(l.keys, v)
				}
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && !isMap {
				if v, ok := strLit(kv.Value); ok {
					l.fields[k.Name] = append(l.fields[k.Name], v)
				}
			} else if v, ok := strLit(kv.Key); ok && isMap {
				l.keys = append(l.keys, v)
			}
		}
		return true
	})
	return l
}

// readRuns reads how fn's body names its subtests.
func readRuns(fn *ast.FuncDecl) *runNames {
	r := &runNames{fields: map[string]bool{}, own: literals{fields: map[string][]string{}}}
	if fn.Body == nil {
		return r
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Run" {
			return true
		}
		switch a := call.Args[0].(type) {
		case *ast.BasicLit:
			if v, ok := strLit(a); ok {
				r.direct = append(r.direct, v)
			}
		case *ast.SelectorExpr:
			r.fields[a.Sel.Name] = true
		case *ast.Ident:
			r.ident = true
		}
		return true
	})
	r.own = collectLiterals(fn.Body)
	return r
}

// declaredNames lists every identifier f declares other than a test function.
func declaredNames(f *ast.File, testFile bool) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.Recv != nil || !testFile || !isTestName(x.Name.Name) {
				out = append(out, x.Name.Name)
			}
		case *ast.TypeSpec:
			out = append(out, x.Name.Name)
		case *ast.ValueSpec:
			for _, id := range x.Names {
				out = append(out, id.Name)
			}
		case *ast.Field:
			for _, id := range x.Names {
				out = append(out, id.Name)
			}
		}
		return true
	})
	return out
}

// readComments returns f's comment groups as plain text.
func readComments(fset *token.FileSet, f *ast.File) []comment {
	var out []comment
	for _, g := range f.Comments {
		var c comment
		var texts []string
		for _, cm := range g.List {
			start := fset.Position(cm.Slash).Line
			for i, l := range strings.Split(cm.Text, "\n") {
				l = strings.TrimPrefix(strings.TrimPrefix(l, "//"), "/*")
				texts = append(texts, l)
				c.lines = append(c.lines, start+i)
			}
		}
		c.text = strings.Join(texts, "\n")
		out = append(out, c)
	}
	return out
}

func parseFile(root, file string) (fileFacts, error) {
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return fileFacts{}, err
	}
	ff := fileFacts{dir: filepath.ToSlash(filepath.Dir(rel)), tests: map[string]*runNames{}}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return ff, err
	}
	testFile := strings.HasSuffix(file, "_test.go")
	if testFile {
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && isTestName(fn.Name.Name) {
				ff.tests[fn.Name.Name] = readRuns(fn)
				continue
			}
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Example") {
				ff.examples = append(ff.examples, fn.Name.Name) // go test -run matches these too
			}
			ff.outside.add(collectLiterals(decl))
		}
	}
	ff.declared = declaredNames(f, testFile)
	ff.comments = readComments(fset, f)
	return ff, nil
}

// goFiles lists the .go files of the five modules under root.
func goFiles(root string) ([]string, error) {
	var out []string
	for _, m := range Modules {
		dir := filepath.Join(root, m)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if p != dir && (strings.HasPrefix(d.Name(), ".") || skipDirs[d.Name()]) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(d.Name(), ".go") {
				out = append(out, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// citation is one test a comment cites.
type citation struct {
	name       string   // the top-level test
	path       []string // subtest segments, possibly none
	text       string   // as written
	start, end int      // byte offsets of text in the comment
	grave      bool     // the words touching it say the test is gone
	run        bool     // a -run argument: a pattern any test name may contain
	pin        bool     // after a PIN: marker, which names a test by contract
}

// citations returns the test citations in a comment's text.
func citations(text string) []citation {
	var out []citation
	for _, m := range citeRe.FindAllStringSubmatchIndex(text, -1) {
		pin := m[2] >= 0
		name := text[m[4]:m[5]]
		if !pin && !strings.HasPrefix(name, "Test") {
			continue // Fuzz and Benchmark are read after a PIN: marker only
		}
		if m[4] > 0 && text[m[4]-1] == '.' {
			continue // a selector: some package's name, not a test
		}
		path := strings.TrimRight(text[m[6]:m[7]], ".") // a sentence's dot, or `...`
		end := m[6] + len(path)
		if path == "" && (strings.HasPrefix(text[end:], "*") || strings.HasPrefix(text[end:], ".*")) {
			continue // a -run glob, not one test
		}
		if strings.HasSuffix(name, "_") || name == "TestMain" {
			continue
		}
		c := citation{name: name, text: name + path, start: m[4], end: end, grave: gravestone(text[:m[4]], text[end:]),
			run: runArg(text[:m[4]]) && path == "" && !strings.HasPrefix(text[end:], "$"), pin: pin}
		for _, seg := range strings.Split(path, "/") {
			if seg != "" {
				c.path = append(c.path, seg)
			}
		}
		out = append(out, c)
	}
	return out
}

// gravestone reports whether the words touching a citation say the test is
// gone; before and after are the comment text on either side of it.
func gravestone(before, after string) bool {
	b := strings.ToLower(strings.TrimRight(before, " \t\n(\"'`"))
	a := strings.ToLower(strings.TrimLeft(after, " \t\n)\"'`,"))
	a = strings.Join(strings.Fields(a), " ") // a phrase may wrap
	if len(a) > 40 {
		a = a[:40]
	}
	b = strings.Join(strings.Fields(b), " ")
	for _, w := range graveBefore {
		if strings.HasSuffix(b, w) && (len(b) == len(w) || !isLetter(b[len(b)-len(w)-1])) {
			return true
		}
	}
	for _, w := range graveAfter {
		if strings.HasPrefix(a, w) && (len(a) == len(w) || !isLetter(a[len(w)])) {
			return true
		}
	}
	return false
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// Tree is the test population of the five modules and the comments citing it.
type Tree struct {
	tests    map[string][]string // test name -> package dirs declaring it
	pkgs     map[string]*pkgFacts
	declared map[string]bool
	examples map[string]bool
	comments map[string][]comment // file -> comment groups
}

// Load reads every Go file of the five modules under root.
func Load(root string) (*Tree, error) {
	files, err := goFiles(root)
	if err != nil {
		return nil, err
	}
	t := &Tree{
		tests:    map[string][]string{},
		pkgs:     map[string]*pkgFacts{},
		declared: map[string]bool{},
		examples: map[string]bool{},
		comments: map[string][]comment{},
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		sem      = make(chan struct{}, 16)
	)
	for _, p := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(p string) {
			defer func() { <-sem; wg.Done() }()
			ff, err := parseFile(root, p)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			t.add(root, p, ff)
		}(p)
	}
	wg.Wait()
	return t, firstErr
}

func (t *Tree) add(root, file string, ff fileFacts) {
	pf := t.pkgs[ff.dir]
	if pf == nil {
		pf = &pkgFacts{tests: map[string]*runNames{}}
		t.pkgs[ff.dir] = pf
	}
	for name, r := range ff.tests {
		if have := pf.tests[name]; have != nil {
			have.merge(r)
		} else {
			pf.tests[name] = r
			t.tests[name] = append(t.tests[name], ff.dir)
		}
	}
	pf.outside.add(ff.outside)
	for _, d := range ff.declared {
		t.declared[d] = true
	}
	for _, e := range ff.examples {
		t.examples[e] = true
	}
	rel, _ := filepath.Rel(root, file)
	t.comments[filepath.ToSlash(rel)] = ff.comments
}

// resolve says why c does not resolve, or "" when it does.
func (t *Tree) resolve(c citation) string {
	if c.run {
		for name := range t.tests {
			if strings.Contains(name, c.name) {
				return ""
			}
		}
		return "is a -run pattern no test name contains"
	}
	dirs, ok := t.tests[c.name]
	if !ok {
		return "is cited but no such test exists"
	}
	if len(c.path) == 0 {
		return ""
	}
	for _, d := range dirs { // any package declaring the parent may own the case
		pf := t.pkgs[d]
		names := pf.tests[c.name].names(pf.outside)
		found := true
		for _, seg := range c.path {
			found = found && names[seg]
		}
		if found {
			return ""
		}
	}
	return fmt.Sprintf("is cited but %s has no subtest %q that can be read statically", c.name, strings.Join(c.path, "/"))
}

// Scan returns every dangling test citation in the five modules under root,
// and every gravestone that names a test no longer there, each sorted by file
// and line. Only the first fails the check; the second is for reading.
func Scan(root string) (hits, graves []Hit, err error) {
	t, err := Load(root)
	if err != nil {
		return nil, nil, err
	}
	for file, groups := range t.comments {
		for _, g := range groups {
			for _, c := range citations(g.text) {
				if _, isTest := t.tests[c.name]; !isTest && t.declared[c.name] && !c.pin {
					continue // a symbol spelled like a test; a PIN: names a test
				}
				why := t.resolve(c)
				switch {
				case why == "":
				case c.grave && !c.pin:
					graves = append(graves, Hit{file, g.lineAt(c.start), c.text, "is a gravestone"})
				default:
					hits = append(hits, Hit{file, g.lineAt(c.start), c.text, why})
				}
			}
		}
	}
	sortHits(hits)
	sortHits(graves)
	return hits, graves, nil
}

func sortHits(hits []Hit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		if hits[i].Line != hits[j].Line {
			return hits[i].Line < hits[j].Line
		}
		return hits[i].Cite < hits[j].Cite
	})
}

func main() {
	root := flag.String("root", ".", "repo root holding the five modules")
	listGraves := flag.Bool("graves", false, "also list the gravestones the check exempts")
	docs := flag.Bool("docs", false, "read the in-scope markdown docs instead of Go comments (docs.go)")
	flag.Parse()
	scan := Scan
	if *docs {
		scan = ScanDocs
	}
	hits, graves, err := scan(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testcite:", err)
		os.Exit(2)
	}
	if *listGraves {
		for _, g := range graves {
			fmt.Println(g)
		}
	}
	for _, h := range hits {
		fmt.Println(h)
	}
	if len(hits) > 0 {
		if *docs {
			fmt.Println(docsSummary(len(hits)))
		} else {
			fmt.Printf("%d dangling test citation(s). Point the comment at the test that pins it now, or say the pin is gone.\n", len(hits))
		}
		os.Exit(1)
	}
}

// runArg reports whether the text before a citation ends in a go test -run
// flag, so the citation is a pattern and not one name (a -run of a name also
// matches every longer name it begins). A trailing `$` anchors it back to one.
func runArg(before string) bool {
	b := strings.TrimRight(before, " \t\n'\"=^")
	return strings.HasSuffix(b, "-run") || strings.HasSuffix(b, "-test.run")
}
