package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strings"
	"sync"
)

// --- serial-tests ----------------------------------------------------------
//
// In the packages this rule covers, a top-level test either calls t.Parallel()
// or says why it does not, on the line directly above its func:
//
//	// serial: it sets the process clock (clock.SetDefault), which every engine's loops read
//	func TestForecast_TheProjectionIsWallTime(t *testing.T) {
//
// WHY. A test that does not call t.Parallel() runs alone, and every serial top
// in a package is a link in a chain the whole package waits for: Core engine's
// 60 serial tops were 24.1 s of a 44.7 s package (MEASURED, the gate-time
// baseline of 2026-11-22), and most had no reason a reader could find. The
// cost of omission is invisible to the author and is paid by every gate run
// after. A reason on the line makes the cost a decision someone wrote down,
// and a reviewer can check it against the code.
//
// WHAT IS READ. The scope file, serialScopeFile: its `package <dir>` lines are
// the directories whose tracked *_test.go files are read (that directory only,
// not the ones under it), whatever their build tags. In them, each top-level
// func named Test* (TestMain excepted) with one *testing.T parameter is
// parallel when its own body calls <param>.Parallel() outside any func literal
// (a subtest's t.Parallel() does not make its parent parallel).
//
// WHAT FIRES, per test in scope:
//   - serial, no marker, and no `unaudited` line for it: the new test that
//     omitted it;
//   - a marker with no reason after the colon;
//   - a marker on a test that calls t.Parallel(): the comment is false;
//   - an `unaudited` line whose test is parallel or marked now, names no test,
//     or names a directory not in scope: the list only shrinks, and a stale
//     line would hide the next serial test of that name.
//
// HOW IT GROWS. A package joins with a `package` line and an `unaudited` line
// for each serial top it already has (the rule's findings name them, 30 at a
// time). A test leaves the list when someone audits it: it goes parallel, or
// it gets a marker naming what it shares. Packages not listed are not read, so
// the rule never asks for markers nobody has audited.
const serialScopeFile = "shared/scripts/repolint/serial-tests.txt"

// serialMarker starts the line that says why a test is serial.
const serialMarker = "// serial:"

// serialScope is the scope file read: the packages in scope and the serial
// tops in them that predate the rule, each with the line that names it.
type serialScope struct {
	packages  map[string]bool
	unaudited map[string]int // "<dir> <test>" -> line in the scope file
}

func readSerialScope(t *tree) (*serialScope, []Finding) {
	var data []byte
	found := false
	for _, f := range t.files {
		if f.path == serialScopeFile {
			data, found = f.data, f.data != nil
		}
	}
	if !found {
		return nil, []Finding{{File: serialScopeFile, Why: "the scope file is not in the tree, so no package is checked"}}
	}
	s := &serialScope{packages: map[string]bool{}, unaudited: map[string]int{}}
	var bad []Finding
	eachLine(data, func(n int, line []byte) {
		l := strings.TrimSpace(string(line))
		if l == "" || strings.HasPrefix(l, "#") {
			return
		}
		fs := strings.Fields(l)
		switch {
		case fs[0] == "package" && len(fs) == 2:
			s.packages[fs[1]] = true
		case fs[0] == "unaudited" && len(fs) == 3:
			key := fs[1] + " " + fs[2]
			if _, dup := s.unaudited[key]; dup {
				bad = append(bad, Finding{File: serialScopeFile, Line: n, Why: "names " + key + " twice"})
			}
			s.unaudited[key] = n
		default:
			bad = append(bad, Finding{File: serialScopeFile, Line: n,
				Why: "is neither `package <dir>` nor `unaudited <dir> <test>`"})
		}
	})
	return s, bad
}

// topTest is one top-level test the rule read.
type topTest struct {
	file, name string
	line       int
	parallel   bool
	marked     bool   // the line above func starts with serialMarker
	reason     string // what follows the marker
}

// topTests returns the top-level tests of one test file.
func topTests(f *file) ([]topTest, error) {
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, f.path, f.data, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(f.data, []byte("\n"))
	var out []topTest
	for _, d := range af.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Body == nil || !strings.HasPrefix(fd.Name.Name, "Test") || fd.Name.Name == "TestMain" {
			continue
		}
		param, ok := testingTParam(fd)
		if !ok {
			continue
		}
		tt := topTest{file: f.path, name: fd.Name.Name, line: fset.Position(fd.Pos()).Line}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if _, lit := n.(*ast.FuncLit); lit {
				return false
			}
			if c, ok := n.(*ast.CallExpr); ok && len(c.Args) == 0 {
				if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "Parallel" {
					if id, ok := s.X.(*ast.Ident); ok && id.Name == param {
						tt.parallel = true
					}
				}
			}
			return true
		})
		if i := tt.line - 2; i >= 0 && i < len(lines) {
			above := strings.TrimSpace(strings.TrimRight(string(lines[i]), "\r"))
			if rest, ok := strings.CutPrefix(above, serialMarker); ok {
				tt.marked, tt.reason = true, strings.TrimSpace(rest)
			}
		}
		out = append(out, tt)
	}
	return out, nil
}

// testingTParam is the name of fd's one parameter when it is a *testing.T.
func testingTParam(fd *ast.FuncDecl) (string, bool) {
	ps := fd.Type.Params.List
	if len(ps) != 1 || len(ps[0].Names) != 1 {
		return "", false
	}
	star, ok := ps[0].Type.(*ast.StarExpr)
	if !ok {
		return "", false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "T" {
		return "", false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "testing" {
		return "", false
	}
	return ps[0].Names[0].Name, true
}

func serialTests(t *tree) result {
	scope, fs := readSerialScope(t)
	if scope == nil {
		return result{findings: fs}
	}
	var files []*file
	for _, f := range t.files {
		if f.data != nil && strings.HasSuffix(f.path, "_test.go") && scope.packages[path.Dir(f.path)] &&
			bytes.Contains(f.data, []byte("func Test")) {
			files = append(files, f)
		}
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		tests []topTest
		sem   = make(chan struct{}, 16)
	)
	for _, f := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(f *file) {
			defer func() { <-sem; wg.Done() }()
			got, err := topTests(f)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fs = append(fs, Finding{File: f.path, Why: "does not parse, so its tests cannot be read: " + err.Error()})
				return
			}
			tests = append(tests, got...)
		}(f)
	}
	wg.Wait()
	sort.Slice(tests, func(i, j int) bool { return tests[i].file+tests[i].name < tests[j].file+tests[j].name })

	seen := map[string]bool{}
	var parallel, marked, unaudited int
	for _, tt := range tests {
		key := path.Dir(tt.file) + " " + tt.name
		listed := scope.unaudited[key] > 0
		seen[key] = true
		switch {
		case tt.parallel && tt.marked:
			fs = append(fs, Finding{File: tt.file, Line: tt.line,
				Why: tt.name + " calls t.Parallel() under a `" + serialMarker + "` marker: the comment is false"})
		case tt.marked && tt.reason == "":
			fs = append(fs, Finding{File: tt.file, Line: tt.line,
				Why: tt.name + "'s `" + serialMarker + "` marker gives no reason"})
		case !tt.parallel && !tt.marked && !listed:
			fs = append(fs, Finding{File: tt.file, Line: tt.line,
				Why: tt.name + " is serial and does not say why"})
		}
		switch {
		case listed && (tt.parallel || tt.marked):
			fs = append(fs, Finding{File: serialScopeFile, Line: scope.unaudited[key],
				Why: "unaudited " + key + ": the test is audited now (parallel or marked), so the line goes"})
		case tt.parallel:
			parallel++
		case tt.marked:
			marked++
		default:
			unaudited++
		}
	}
	for key, n := range scope.unaudited {
		dir, _, _ := strings.Cut(key, " ")
		switch {
		case !scope.packages[dir]:
			fs = append(fs, Finding{File: serialScopeFile, Line: n,
				Why: "unaudited " + key + ": " + dir + " has no `package` line, so nothing reads it"})
		case !seen[key]:
			fs = append(fs, Finding{File: serialScopeFile, Line: n,
				Why: "unaudited " + key + ": no top-level test of that name in " + dir + ", so the line goes"})
		}
	}
	return result{fs, fmt.Sprintf("%d packages, %d top-level tests: %d parallel, %d marked serial, %d unaudited",
		len(scope.packages), len(tests), parallel, marked, unaudited)}
}
