// Package testset is the one answer to "which tests are there, at a base and
// at the tip": top-level test functions keyed by package directory and name,
// build tags ignored, the base a commit and the tip the working tree.
//
// Two guards read it. testledger (scripts/check-test-ledger.sh) fails a test
// the base has and the tip does not, unless the ledger names it; testcost
// (scripts/check-test-cost.sh) times a test the tip has and the base does not.
// One notion of a test and of the base, so the two cannot disagree about what
// was there before.
package testset

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Modules are the five Go modules whose tests are counted.
var Modules = []string{"protocol", "shared", "shingo-core", "shingo-edge", "integration"}

// Git runs git in root and returns its stdout.
func Git(root string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ResolveBase returns the commit to compare against: ref when given, else
// the merge base of HEAD and origin/main.
func ResolveBase(root, ref string) (string, error) {
	if ref == "" {
		out, err := Git(root, "merge-base", "HEAD", "origin/main")
		if err != nil {
			return "", fmt.Errorf("no base: %v (set -base or TEST_LEDGER_BASE, or fetch origin/main)", err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	out, err := Git(root, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// IsTestFile reports whether a slash path is a _test.go file go test reads.
func IsTestFile(p string) bool {
	if !strings.HasSuffix(p, "_test.go") {
		return false
	}
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if seg == "testdata" || seg == "vendor" || seg == "node_modules" || strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, "_") {
			return false
		}
	}
	base := path.Base(p)
	return !strings.HasPrefix(base, ".") && !strings.HasPrefix(base, "_")
}

// IsTestName is go test's rule for a test function's name: Test, then nothing or a
// rune that is not lower case (Testable is a helper). TestMain is the package's
// test entry point, not a test.
func IsTestName(n string) bool {
	if n == "TestMain" || !strings.HasPrefix(n, "Test") {
		return false
	}
	rest := n[len("Test"):]
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(r)
}

// TestFuncs returns the top-level Test functions src declares (no receiver),
// with their lines.
func TestFuncs(name string, src []byte) (map[string]int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && IsTestName(fn.Name.Name) {
			out[fn.Name.Name] = fset.Position(fn.Pos()).Line
		}
	}
	return out, nil
}

// ChangedTestFiles lists the tracked _test.go files of the modules that
// differ between base and the working tree.
func ChangedTestFiles(root, base string) ([]string, error) {
	out, err := Git(root, append([]string{"diff", "--name-only", "--no-renames", base, "--"}, Modules...)...)
	if err != nil {
		return nil, err
	}
	return testFilesOf(out), nil
}

func testFilesOf(listing []byte) []string {
	var files []string
	for _, l := range strings.Split(string(listing), "\n") {
		if l = strings.TrimSpace(l); IsTestFile(l) {
			files = append(files, l)
		}
	}
	return files
}

// ReadAtBase returns the base's content of each file it has, by path.
func ReadAtBase(root, base string, files []string) (map[string][]byte, error) {
	cmd := exec.Command("git", "-C", root, "cat-file", "--batch")
	var in bytes.Buffer
	for _, f := range files {
		fmt.Fprintf(&in, "%s:%s\n", base, f)
	}
	cmd.Stdin = &in
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %v", err)
	}
	out := map[string][]byte{}
	r := bufio.NewReader(bytes.NewReader(raw))
	for _, f := range files {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("git cat-file: %s: %v", f, err)
		}
		fields := strings.Fields(header)
		if len(fields) == 2 && fields[1] == "missing" {
			continue // added since the base
		}
		var size int
		if len(fields) != 3 {
			return nil, fmt.Errorf("git cat-file: %s: bad header %q", f, header)
		}
		if _, err := fmt.Sscan(fields[2], &size); err != nil {
			return nil, fmt.Errorf("git cat-file: %s: bad size in %q", f, header)
		}
		body := make([]byte, size+1) // the body and its trailing newline
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("git cat-file: %s: %v", f, err)
		}
		out[f] = body[:size]
	}
	return out, nil
}

// TipTests returns the Test functions every _test.go file in dir declares
// in the working tree, with the file and line of each.
func TipTests(root, dir string) (map[string]Test, error) {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Test{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]Test{}
	for _, e := range entries {
		p := path.Join(dir, e.Name())
		if e.IsDir() || !IsTestFile(p) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			return nil, err
		}
		fns, err := TestFuncs(p, src)
		if err != nil {
			return nil, err
		}
		for n, line := range fns {
			out[n] = Test{Dir: dir, Name: n, File: p, Line: line}
		}
	}
	return out, nil
}

// Test is one top-level test.
type Test struct {
	Dir  string // package directory, slash-separated from the repo root
	Name string
	File string // the file that declares it
	Line int
}

// Added returns the tests the working tree declares that base does not, by
// package directory and name, sorted. A test moved to another file of its
// package is not added; one renamed or moved to another package is. Read: the
// directories that hold a _test.go file changed since base (tracked) or not
// yet tracked; in each, every _test.go file at the tip and at the base.
func Added(root, base string) ([]Test, error) {
	changed, err := ChangedTestFiles(root, base)
	if err != nil {
		return nil, err
	}
	out, err := Git(root, append([]string{"ls-files", "--others", "--exclude-standard", "--"}, Modules...)...)
	if err != nil {
		return nil, err
	}
	dirs := map[string]bool{}
	for _, f := range append(changed, testFilesOf(out)...) {
		dirs[path.Dir(f)] = true
	}
	var added []Test
	for dir := range dirs {
		tip, err := TipTests(root, dir)
		if err != nil {
			return nil, err
		}
		listing, err := Git(root, "ls-tree", "--name-only", base, "--", dir+"/")
		if err != nil {
			return nil, err
		}
		var baseFiles []string
		for _, f := range testFilesOf(listing) {
			if path.Dir(f) == dir {
				baseFiles = append(baseFiles, f)
			}
		}
		srcs, err := ReadAtBase(root, base, baseFiles)
		if err != nil {
			return nil, err
		}
		atBase := map[string]bool{}
		for f, src := range srcs {
			fns, err := TestFuncs(f, src)
			if err != nil {
				return nil, fmt.Errorf("%s at %s: %v", f, base, err)
			}
			for n := range fns {
				atBase[n] = true
			}
		}
		for n, tt := range tip {
			if !atBase[n] {
				added = append(added, tt)
			}
		}
	}
	sort.Slice(added, func(i, j int) bool {
		if added[i].Dir != added[j].Dir {
			return added[i].Dir < added[j].Dir
		}
		return added[i].Name < added[j].Name
	})
	return added, nil
}

// CostMarker starts the line of a top-level test's doc comment that says why
// the test costs more than the 10 s line, run alone: what needs the time, and
// what a cheaper tier cannot assert (docs/ground-rules.md). The repolint's
// test-costs rule reads where it stands; testcost reads which tests carry one.
const CostMarker = "// cost:"

// CostLine is one CostMarker line of a test file.
type CostLine struct {
	Line   int
	Test   string // the top-level test whose doc comment holds it, "" when none does
	Reason string // what follows the marker
}

// CostLines returns every CostMarker line in src's comments, in order.
func CostLines(name string, src []byte) ([]CostLine, error) {
	if !bytes.Contains(src, []byte(CostMarker)) {
		return nil, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	docOf := map[*ast.CommentGroup]string{}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Doc != nil && IsTestName(fn.Name.Name) {
			docOf[fn.Doc] = fn.Name.Name
		}
	}
	var out []CostLine
	for _, g := range f.Comments {
		for _, c := range g.List {
			if rest, ok := strings.CutPrefix(c.Text, CostMarker); ok {
				out = append(out, CostLine{Line: fset.Position(c.Pos()).Line, Test: docOf[g], Reason: strings.TrimSpace(rest)})
			}
		}
	}
	return out, nil
}
