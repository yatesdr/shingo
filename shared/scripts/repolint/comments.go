package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// --- comment-references ----------------------------------------------------
//
// What a comment CITES must still be there: a file:line citation in a Go
// comment must name a file that exists, at a line inside it.
//
// WHY THIS IS THE HALF THAT LASTS. This codebase reasons in its comments, and it
// has been burned every time one went quietly false: a six-mode census in
// swap_leg_role.go written with a file:line per row, every one of which rotted
// (by +43 in one file, +88 in another, by MINUS 58 and 71 in a third), and a
// correction that called the drift "uniformly +43" that was itself wrong,
// because nobody could check it mechanically. A citation past the end of its
// file is unambiguously rotten, and checkable on the commit that breaks it.
//
// WHAT IT DOES NOT CHECK, on purpose: whether a still-valid line number is the
// RIGHT line (not catchable: cite a SYMBOL for anything you expect to survive);
// bare test names (testcite reads them, PIN: markers included); bare file names
// in prose; arbitrary identifiers (deadident reads the code-shaped ones).
//
// WHAT IS READ. Full-line // comments (a line that is only space before the
// //) of the tracked .go files of the five modules, as the guard's grep read
// them: a trailing comment after code and a /* */ block are not read, and a
// raw string line that starts with // is.
//
// RESOLVE ON A PATH-COMPONENT BOUNDARY, three ways, in this order. All three
// shapes occur in the tree, and the first cut of the guard got every one wrong:
//
//	exact     a path from the repo root, against every tracked .go file.
//	suffix    a trailing path fragment ("edge/engine/changeover.go" is how the
//	          codebase abbreviates shingo-edge), against the five modules.
//	basename  the whole last component, NOT a suffix of it: a suffix match
//	          resolved reshuffle.go to complex_reshuffle.go, a different, shorter
//	          file, and reported a good citation as past the end.
//
// EVERY MATCH, NOT THE FIRST. A suffix or a basename can name several files,
// and "the first" is whichever a directory walk reaches first: alphabetical on
// NTFS, hash order on ext4. A citation to compound.go passed on Windows
// (dispatch's) and failed in CI (internal/testdb's, a tenth the length). The
// line must fit in the longest of the files it could name, the same answer on
// every OS. A line count is the file's newlines, as wc -l counts them.
var (
	fullCommentRe = regexp.MustCompile(`^[[:space:]]*//`)
	fileLineRe    = regexp.MustCompile(`[a-zA-Z0-9_/-]+[.]go:[0-9]+`)

	// allowedMissing are files a citation may name that are not in this tree.
	// Each needs a reason: an entry without one is indistinguishable from
	// silencing a failure.
	allowedMissing = map[string]string{
		"migrateloaders.go":     "the deleted command; comments cite it BECAUSE it is gone",
		"sqlite_linux_amd64.go": "modernc.org/sqlite, a dependency; the citation is into vendored source, not this tree",
	}
)

// inModule reports whether a repo path lies in one of the five modules.
func inModule(p string) bool {
	top, _, _ := strings.Cut(p, "/")
	for _, m := range Modules {
		if top == m {
			return true
		}
	}
	return false
}

// goIndex is every tracked .go file and its line count.
type goIndex struct {
	lines map[string]int
	paths []string // in path order
}

// resolve says why a citation cannot be right, or "" when it can.
func (ix goIndex) resolve(cite string) string {
	i := strings.LastIndexByte(cite, ':')
	f := cite[:i]
	ln, err := strconv.Atoi(cite[i+1:])
	if err != nil {
		ln = int(^uint(0) >> 1) // more digits than an int holds: past any end
	}
	bn := f[strings.LastIndexByte(f, '/')+1:]
	if _, ok := allowedMissing[bn]; ok {
		return ""
	}
	var cand []string
	if _, ok := ix.lines[f]; ok {
		cand = append(cand, f)
	}
	if len(cand) == 0 && strings.Contains(f, "/") {
		for _, p := range ix.paths {
			if inModule(p) && len(p) > len(f) && strings.HasSuffix(p, "/"+f) {
				cand = append(cand, p)
			}
		}
	}
	if len(cand) == 0 {
		for _, p := range ix.paths {
			if inModule(p) && p[strings.LastIndexByte(p, '/')+1:] == bn {
				cand = append(cand, p)
			}
		}
	}
	if len(cand) == 0 {
		return "no such file"
	}
	longest, total := "", 0
	for _, c := range cand {
		if ix.lines[c] > total {
			longest, total = c, ix.lines[c]
		}
	}
	if ln > total {
		return fmt.Sprintf("%s has only %d lines", longest, total)
	}
	return ""
}

func commentReferences(t *tree) result {
	ix := goIndex{lines: map[string]int{}}
	for _, f := range t.files {
		if strings.HasSuffix(f.path, ".go") && f.data != nil {
			ix.lines[f.path] = bytes.Count(f.data, []byte{'\n'})
			ix.paths = append(ix.paths, f.path)
		}
	}
	verdict := map[string]string{} // one resolution per distinct citation
	var fs []Finding
	for _, f := range t.files {
		if !strings.HasSuffix(f.path, ".go") || f.data == nil || !inModule(f.path) {
			continue
		}
		eachLine(f.data, func(n int, line []byte) {
			if !bytes.Contains(line, []byte(".go:")) || !fullCommentRe.Match(line) {
				return
			}
			seen := map[string]bool{}
			for _, c := range fileLineRe.FindAllString(string(line), -1) {
				why, ok := verdict[c]
				if !ok {
					why = ix.resolve(c)
					verdict[c] = why
				}
				if why != "" && !seen[c] {
					seen[c] = true
					fs = append(fs, Finding{File: f.path, Line: n, Why: fmt.Sprintf("cites %s -- %s", c, why)})
				}
			}
		})
	}
	return result{fs, "file:line citations resolve"}
}

// --- expiry ----------------------------------------------------------------
//
// A scar may name its expiry as a symbol, in a Go comment of the five modules:
// EXPIRY: then "when", the symbol, "is gone" (the symbol may be backticked and
// the phrase may wrap). The run fails when nothing in the five modules declares
// that symbol any more, so the scar is retired in the same edit that deletes the
// symbol (AGENTS.md: correct the scar in the same edit that changes the
// behaviour). No date-based expiry: a date fails the gate for a reason outside
// the diff.
//
// A SYMBOL is Name, pkg.Name, Type.Member, pkg.Type.Member or (*Type).Member,
// with an optional trailing (). Declared means, build tags ignored and test
// files included: Name is declared at top level, as a method, field or
// interface method anywhere; pkg.Name is a top-level declaration of a package
// of that name; Type.Member is a method, field or interface method of a type of
// that name. testdata and vendor trees are not read: Go builds neither.
//
// An EXPIRY line in any other form is prose, and is not read.
const ident = `[A-Za-z_][A-Za-z0-9_]*`

var expiryRe = regexp.MustCompile("EXPIRY:\\s*when\\s+`?((?:\\(\\*?" + ident + "(?:\\." + ident + ")?\\)|\\*?" + ident + ")(?:\\." +
	ident + "){0,2}(?:\\(\\))?)`?\\s+is\\s+gone\\b")

// pin is one EXPIRY-by-symbol marker.
type pin struct {
	file   string
	line   int
	symbol string
}

// decls is what the five modules declare.
type decls struct {
	any     map[string]bool            // every declared name
	top     map[string]map[string]bool // package name -> its top-level names
	members map[string]map[string]bool // type name -> its methods and fields
}

func (d *decls) add(m map[string]map[string]bool, k, name string) {
	if m[k] == nil {
		m[k] = map[string]bool{}
	}
	m[k][name] = true
	d.any[name] = true
}

// declares reports whether sym is declared, by the forms above.
func (d *decls) declares(sym string) bool {
	sym = strings.TrimSuffix(sym, "()")
	if strings.HasPrefix(sym, "(") { // (*T).M
		inner, rest, _ := strings.Cut(sym[1:], ")")
		sym = strings.TrimPrefix(inner, "*") + rest
	}
	parts := strings.Split(strings.TrimPrefix(sym, "*"), ".")
	switch len(parts) {
	case 1:
		return d.any[parts[0]]
	case 2:
		return d.top[parts[0]][parts[1]] || d.members[parts[0]][parts[1]]
	case 3:
		return d.top[parts[0]][parts[1]] && d.members[parts[1]][parts[2]]
	}
	return false
}

// typeName is the base name of a receiver or embedded type.
func typeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return typeName(x.X)
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.IndexExpr:
		return typeName(x.X)
	case *ast.IndexListExpr:
		return typeName(x.X)
	}
	return ""
}

// addFields records a struct's fields or an interface's methods as members of
// the type named owner; an embedded one is named by its type.
func (d *decls) addFields(owner string, fl *ast.FieldList) {
	if fl == nil {
		return
	}
	for _, fd := range fl.List {
		if len(fd.Names) == 0 {
			if n := typeName(fd.Type); n != "" {
				d.add(d.members, owner, n)
			}
		}
		for _, n := range fd.Names {
			d.add(d.members, owner, n.Name)
		}
	}
}

// addFile records one parsed file's declarations.
func (d *decls) addFile(f *ast.File) {
	pkg := f.Name.Name
	for _, decl := range f.Decls {
		switch x := decl.(type) {
		case *ast.FuncDecl:
			if x.Recv == nil || len(x.Recv.List) == 0 {
				d.add(d.top, pkg, x.Name.Name)
			} else {
				d.add(d.members, typeName(x.Recv.List[0].Type), x.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range x.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					d.add(d.top, pkg, s.Name.Name)
					switch tt := s.Type.(type) {
					case *ast.StructType:
						d.addFields(s.Name.Name, tt.Fields)
					case *ast.InterfaceType:
						d.addFields(s.Name.Name, tt.Methods)
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						d.add(d.top, pkg, n.Name)
					}
				}
			}
		}
	}
}

// moduleGo lists the tracked .go files of the five modules that Go builds:
// none under a testdata or vendor directory.
func moduleGo(t *tree) []*file {
	var out []*file
	for _, f := range t.files {
		p := "/" + f.path
		if f.data != nil && strings.HasSuffix(f.path, ".go") && inModule(f.path) &&
			!strings.Contains(p, "/testdata/") && !strings.Contains(p, "/vendor/") {
			out = append(out, f)
		}
	}
	return out
}

// pinsIn returns the EXPIRY-by-symbol pins in one file's comments, reading
// each comment group as one text so a wrapped phrase is read whole.
func pinsIn(f *file) ([]pin, error) {
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, f.path, f.data, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []pin
	for _, g := range af.Comments {
		var texts []string
		var lines []int
		for _, c := range g.List {
			start := fset.Position(c.Slash).Line
			for i, l := range strings.Split(c.Text, "\n") {
				texts = append(texts, strings.TrimPrefix(strings.TrimPrefix(l, "//"), "/*"))
				lines = append(lines, start+i)
			}
		}
		text := strings.Join(texts, "\n")
		for _, m := range expiryRe.FindAllStringSubmatchIndex(text, -1) {
			out = append(out, pin{f.path, lines[strings.Count(text[:m[0]], "\n")], text[m[2]:m[3]]})
		}
	}
	return out, nil
}

// parseAll parses files in parallel and calls each with every parsed file, under
// one lock. It returns the files that do not parse.
func parseAll(files []*file, each func(*file, *ast.File)) []Finding {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		bad []Finding
		sem = make(chan struct{}, 16)
	)
	for _, f := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(f *file) {
			defer func() { <-sem; wg.Done() }()
			af, err := parser.ParseFile(token.NewFileSet(), f.path, f.data, parser.SkipObjectResolution)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				bad = append(bad, Finding{File: f.path, Why: "does not parse, so what it declares is unknown: " + err.Error()})
				return
			}
			each(f, af)
		}(f)
	}
	wg.Wait()
	return bad
}

func expiry(t *tree) result {
	files := moduleGo(t)
	var pins []pin
	var fs []Finding
	for _, f := range files {
		if !bytes.Contains(f.data, []byte("EXPIRY:")) {
			continue
		}
		p, err := pinsIn(f)
		if err != nil {
			fs = append(fs, Finding{File: f.path, Why: "does not parse, so its EXPIRY pins cannot be read: " + err.Error()})
		}
		pins = append(pins, p...)
	}
	if len(pins) > 0 { // the declarations are read only when a pin needs them
		d := &decls{any: map[string]bool{}, top: map[string]map[string]bool{}, members: map[string]map[string]bool{}}
		fs = append(fs, parseAll(files, func(_ *file, af *ast.File) { d.addFile(af) })...)
		for _, p := range pins {
			if !d.declares(p.symbol) {
				fs = append(fs, Finding{File: p.file, Line: p.line,
					Why: fmt.Sprintf("EXPIRY names %s, which nothing in the five modules declares now: the pin has expired", p.symbol)})
			}
		}
	}
	return result{fs, fmt.Sprintf("%d EXPIRY-by-symbol pins, each symbol still declared", len(pins))}
}
