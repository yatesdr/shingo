// Command deadident reports identifiers that Go comments cite but that no
// code in the repo declares or uses.
//
// WHY. A comment that names a function which was later renamed sends the next
// reader to grep for something that is not there, and nothing fails when it
// happens. Every such citation so far was found by accident.
//
// WHAT COUNTS AS A CITATION. Only shapes that are plainly code: a call
// `Foo(`, a selector `x.Foo`, or an identifier inside a code-like backtick
// span; and, for the retired names below only, a bare word in prose. A name
// must be at least 8 characters and mixed case, and may hold an
// underscore only as a Test/Benchmark/Fuzz name; that drops package names, SQL
// columns, PLC tag names, acronyms and ordinary words.
//
// WHAT COUNTS AS LIVE. Any identifier in the repo's Go ASTs (declared or
// used, build tags ignored), any non-test Go string literal or struct tag value
// that is itself an identifier (template funcs, JSON keys, event names), and any
// identifier token in the repo's own .js and .html files. A selector on a
// package the repo imports from outside itself (`http.ServeContent`) is not
// read: the repo cannot vouch for another module's names.
//
// GRAVESTONES ARE EXEMPT. A comment line that says the thing is gone
// ("removed", "deleted", "retired", "used to", "renamed", ...) cites a dead
// name on purpose; it records history and is never a hit.
//
// Usage: go run ./scripts/deadident [-root DIR]   (exit 1 on a hit)
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

var (
	identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	wordRe  = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*\(?`)
	tickRe  = regexp.MustCompile("`([^`]+)`")
	versRe  = regexp.MustCompile(`^v[0-9]+$`)
	jsCmtRe = regexp.MustCompile(`(?s)/\*.*?\*/|//[^\n]*|<!--.*?-->`)
	graveRe = regexp.MustCompile(`(?i)\b(removed|deleted|retired|used to|renamed|formerly|previously|no longer|replaced|superseded|obsolete|gone|was called|dropped)\b|\bpre-`)
)

// retired names were renamed or folded away, and their citations are cited
// bare in prose, which pattern mode does not read. Each is a hit wherever it
// appears outside a gravestone line, whatever the live set says.
var retired = map[string]bool{
	"planRetrieveEmpty":             true, // PlanningService.planTransport
	"planRetrieve":                  true, // PlanningService.planTransport
	"ApplyBinArrival":               true, // BinService.ApplyArrival
	"handleLoaderEmptyInCompletion": true, // applyLoaderEmptyIn
	"handleManualSwapCompletion":    true, // applyManualSwap
	"fireThresholdL1":               true,
}

// repoModules are the import-path roots that belong to this repo.
var repoModules = []string{"shingo/", "shingocore", "shingoedge"}

// Hit is one dead citation.
type Hit struct {
	File string
	Line int
	Name string
}

type commentLine struct {
	file string
	line int
	text string
}

// qualifies reports whether a name is long and mixed-case enough to be read
// as a citation rather than a word.
func qualifies(name string) bool {
	if len(name) < 8 || strings.HasSuffix(name, "_") {
		return false
	}
	if strings.Contains(name, "_") && !strings.HasPrefix(name, "Test") &&
		!strings.HasPrefix(name, "Benchmark") && !strings.HasPrefix(name, "Fuzz") {
		return false
	}
	var up, low bool
	for _, r := range name {
		up = up || unicode.IsUpper(r)
		low = low || unicode.IsLower(r)
	}
	return up && low
}

// codeLike reports whether a backtick span is code rather than a quoted
// value such as a node name (`Supermarket Area`).
func codeLike(span string) bool {
	return !strings.Contains(span, " ") || strings.ContainsAny(span, "().=!&[:")
}

// A citation is a name plus the package it is selected from, if any.
type citation struct{ qual, name string }

// citations returns the names a comment line cites, in order.
func citations(text string) []citation {
	var out []citation
	seen := map[string]bool{}
	add := func(qual, s string) {
		if qualifies(s) && !seen[s] {
			seen[s] = true
			out = append(out, citation{qual, s})
		}
	}
	scan := func(s string, inTicks bool) {
		for _, loc := range wordRe.FindAllStringIndex(s, -1) {
			w := s[loc[0]:loc[1]]
			if loc[1] < len(s) && s[loc[1]] == '*' {
				continue // a glob (`protocol.LoaderPositionKind*`), not a name
			}
			call := strings.HasSuffix(w, "(")
			parts := strings.Split(strings.TrimSuffix(w, "("), ".")
			for i, p := range parts {
				if inTicks || call || len(parts) > 1 || retired[p] {
					q := ""
					if i > 0 {
						q = parts[i-1]
					}
					add(q, p)
				}
			}
		}
	}
	for _, m := range tickRe.FindAllStringSubmatch(text, -1) {
		if codeLike(m[1]) {
			scan(m[1], true)
		}
	}
	scan(tickRe.ReplaceAllString(text, " "), false)
	return out
}

// stdlib reports whether qual names a top-level standard library package the
// repo does not import (`mime.ParseMediaType`).
func stdlib(qual string) bool {
	if qual == "" || strings.ToLower(qual) != qual {
		return false
	}
	fi, err := os.Stat(filepath.Join(build.Default.GOROOT, "src", qual))
	return err == nil && fi.IsDir()
}

// isGravestone reports whether a comment line records that something is gone.
func isGravestone(text string) bool { return graveRe.MatchString(text) }

// sources lists the .go files and the .js/.html files under root, skipping
// VCS, vendor, testdata and node_modules trees and minified bundles.
func sources(root string) (goFiles, webFiles []string, err error) {
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		n := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(n, ".") || n == "vendor" || n == "testdata" || n == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(n, ".go"):
			goFiles = append(goFiles, p)
		case strings.HasSuffix(n, ".min.js"):
		case strings.HasSuffix(n, ".js"), strings.HasSuffix(n, ".html"):
			webFiles = append(webFiles, p)
		}
		return nil
	})
	return goFiles, webFiles, err
}

// fileFacts is what one file contributes.
type fileFacts struct {
	live     []string
	external []string
	comments []commentLine
}

// externalName is the package name an out-of-repo import is used by, or "".
func externalName(spec *ast.ImportSpec) string {
	p, err := strconv.Unquote(spec.Path.Value)
	if err != nil {
		return ""
	}
	for _, m := range repoModules {
		if strings.HasPrefix(p, m) {
			return ""
		}
	}
	if spec.Name != nil {
		return spec.Name.Name
	}
	base := path.Base(p)
	if versRe.MatchString(base) {
		base = path.Base(path.Dir(p))
	}
	return base
}

// literalIdent returns a string literal's value if it is one identifier.
func literalIdent(lit *ast.BasicLit, out []string) []string {
	if lit.Kind != token.STRING {
		return out
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return out
	}
	if identRe.FindString(v) == v && v != "" {
		return append(out, v)
	}
	return out
}

// parseGo returns what one Go file contributes.
func parseGo(file string) (fileFacts, error) {
	var ff fileFacts
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return ff, err
	}
	for _, spec := range f.Imports {
		if n := externalName(spec); n != "" {
			ff.external = append(ff.external, n)
		}
	}
	// A test's strings are not code: they name dead identifiers on purpose
	// (this command's own fixtures do), and would mask them repo-wide.
	test := strings.HasSuffix(file, "_test.go")
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			ff.live = append(ff.live, x.Name)
		case *ast.BasicLit:
			if !test {
				ff.live = literalIdent(x, ff.live)
			}
		case *ast.Field:
			if x.Tag != nil {
				ff.live = append(ff.live, identRe.FindAllString(x.Tag.Value, -1)...)
			}
		}
		return true
	})
	for _, g := range f.Comments {
		for _, c := range g.List {
			start := fset.Position(c.Slash).Line
			for i, l := range strings.Split(c.Text, "\n") {
				ff.comments = append(ff.comments, commentLine{file, start + i, l})
			}
		}
	}
	return ff, nil
}

// parseWeb returns every identifier token in a .js or .html file outside
// its comments, so a dead name cited in a JS comment does not count as live.
func parseWeb(file string) (fileFacts, error) {
	b, err := os.ReadFile(file)
	code := jsCmtRe.ReplaceAllString(string(b), " ")
	return fileFacts{live: identRe.FindAllString(code, -1)}, err
}

// collect runs parse over files in parallel and merges the results.
func collect(files []string, parse func(string) (fileFacts, error), live, external map[string]bool) ([]commentLine, error) {
	var comments []commentLine
	var firstErr error
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, p := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(p string) {
			defer func() { <-sem; wg.Done() }()
			ff, err := parse(p)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for _, id := range ff.live {
				live[id] = true
			}
			for _, n := range ff.external {
				external[n] = true
			}
			comments = append(comments, ff.comments...)
		}(p)
	}
	wg.Wait()
	return comments, firstErr
}

// Scan reads every source under root and returns the dead citations in Go
// comments, sorted by file and line.
func Scan(root string) ([]Hit, error) {
	goFiles, webFiles, err := sources(root)
	if err != nil {
		return nil, err
	}
	live, external := map[string]bool{}, map[string]bool{}
	comments, err := collect(goFiles, parseGo, live, external)
	if err != nil {
		return nil, err
	}
	if _, err := collect(webFiles, parseWeb, live, external); err != nil {
		return nil, err
	}
	var hits []Hit
	for _, c := range comments {
		if isGravestone(c.text) {
			continue
		}
		for _, ct := range citations(c.text) {
			if retired[ct.name] || !live[ct.name] && !external[ct.qual] && !stdlib(ct.qual) {
				rel, _ := filepath.Rel(root, c.file)
				hits = append(hits, Hit{filepath.ToSlash(rel), c.line, ct.name})
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		return hits[i].Line < hits[j].Line
	})
	return hits, nil
}

func main() {
	root := flag.String("root", ".", "repo root to scan")
	flag.Parse()
	hits, err := Scan(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "deadident:", err)
		os.Exit(2)
	}
	for _, h := range hits {
		fmt.Printf("%s:%d: %s is cited but not declared anywhere\n", h.File, h.Line, h.Name)
	}
	if len(hits) > 0 {
		fmt.Printf("%d dead identifier citation(s). Point the comment at the live name, or say it is gone.\n", len(hits))
		os.Exit(1)
	}
}
