// Command testledger fails when a test present at the merge base is gone at
// the tip and docs/testing/population-ledger.md carries no row for it.
//
// WHY. A deleted test takes its assertions with it, and a diff that removes
// one reads like any other cleanup. The ledger is where a removal says what
// it was (DUP, FOLD, DEAD, ...), which assertion survives it and the mutation
// that proves so; this check makes the row the price of the removal.
//
// WHAT IS COMPARED. Top-level test functions without a receiver (go test's
// naming rule, so not TestMain and not a Testable helper), build tags
// ignored, keyed by package directory and name. The base is
// `git merge-base HEAD origin/main` unless -base or TEST_LEDGER_BASE names
// another ref. The tip is the working tree, so an uncommitted deletion counts.
// Only the _test.go files that differ between the two are read at the base;
// at the tip every _test.go file in their directories is, so a test moved to
// another file of its package has not vanished, and one moved to another
// package has.
//
// WHAT A ROW IS. A `- test:` line in the ledger: its first backticked token is
// the test's name, and `pkg` followed by a backticked import path names its
// package (the import path, as go test prints it, not the directory).
//
// Usage: go run ./scripts/testledger [-root DIR] [-base REF] [-ledger PATH]
// (exit 1 when a test vanished without a row; the missing rows are printed as
// skeletons to fill in)
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Modules are the five Go modules whose tests the ledger accounts for.
var Modules = []string{"protocol", "shared", "shingo-core", "shingo-edge", "integration"}

// LedgerPath is the ledger's place under the repo root.
const LedgerPath = "docs/testing/population-ledger.md"

var (
	rowNameRe = regexp.MustCompile("^- test: `([^`]+)`")
	rowPkgRe  = regexp.MustCompile("\\bpkg `([^`]+)`")
	moduleRe  = regexp.MustCompile(`(?m)^module\s+(\S+)`)
	buildRe   = regexp.MustCompile(`(?m)^//go:build\s+(.+)$`)
)

// Vanished is a test the base declares and the tip does not.
type Vanished struct {
	Dir  string // package directory, slash-separated from the repo root
	Pkg  string // import path
	Name string
	File string // where the base declared it
	Line int
	Tags string // the base file's //go:build expression, or "none"
}

// key is how a test is identified across base and tip.
type key struct{ dir, name string }

// git runs git in root and returns its stdout.
func git(root string, args ...string) ([]byte, error) {
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
		out, err := git(root, "merge-base", "HEAD", "origin/main")
		if err != nil {
			return "", fmt.Errorf("no base: %v (set -base or TEST_LEDGER_BASE, or fetch origin/main)", err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	out, err := git(root, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// isTestFile reports whether a slash path is a _test.go file go test reads.
func isTestFile(p string) bool {
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

// isTestName is go test's rule for a test function's name: Test, then nothing or a
// rune that is not lower case (Testable is a helper). TestMain is the package's
// test entry point, not a test: removing one needs no row.
func isTestName(n string) bool {
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

// testFuncs returns the top-level Test functions src declares, with their lines.
func testFuncs(name string, src []byte) (map[string]int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && isTestName(fn.Name.Name) {
			out[fn.Name.Name] = fset.Position(fn.Pos()).Line
		}
	}
	return out, nil
}

// changedTestFiles lists the _test.go files of the modules that differ
// between base and the working tree.
func changedTestFiles(root, base string) ([]string, error) {
	out, err := git(root, append([]string{"diff", "--name-only", "--no-renames", base, "--"}, Modules...)...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); isTestFile(l) {
			files = append(files, l)
		}
	}
	return files, nil
}

// readAtBase returns the base's content of each file it has, by path.
func readAtBase(root, base string, files []string) (map[string][]byte, error) {
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

// tipTests returns the Test functions every _test.go file in dir declares
// in the working tree.
func tipTests(root, dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, e := range entries {
		p := path.Join(dir, e.Name())
		if e.IsDir() || !isTestFile(p) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			return nil, err
		}
		fns, err := testFuncs(p, src)
		if err != nil {
			return nil, err
		}
		for n := range fns {
			out[n] = true
		}
	}
	return out, nil
}

// modulePaths maps each module directory to its import path, from go.mod.
func modulePaths(root string) (map[string]string, error) {
	out := map[string]string{}
	for _, m := range Modules {
		b, err := os.ReadFile(filepath.Join(root, m, "go.mod"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		mm := moduleRe.FindSubmatch(b)
		if mm == nil {
			return nil, fmt.Errorf("%s/go.mod: no module line", m)
		}
		out[m] = string(mm[1])
	}
	return out, nil
}

// importPath is dir's import path, or dir itself when no module holds it.
func importPath(mods map[string]string, dir string) string {
	mod, rest, _ := strings.Cut(dir, "/")
	p, ok := mods[mod]
	if !ok {
		return dir
	}
	if rest == "" {
		return p
	}
	return p + "/" + rest
}

// Rows returns the (import path, name) pairs the ledger's `- test:` lines name.
func Rows(ledger []byte) map[[2]string]bool {
	out := map[[2]string]bool{}
	for _, l := range strings.Split(string(ledger), "\n") {
		l = strings.TrimSpace(l)
		name := rowNameRe.FindStringSubmatch(l)
		pkg := rowPkgRe.FindStringSubmatch(l)
		if name != nil && pkg != nil {
			out[[2]string{pkg[1], name[1]}] = true
		}
	}
	return out
}

// Check returns the tests the base declares, the tip does not, and the
// ledger has no row for, sorted by package and name.
func Check(root, base string, ledger []byte) ([]Vanished, error) {
	files, err := changedTestFiles(root, base)
	if err != nil {
		return nil, err
	}
	srcs, err := readAtBase(root, base, files)
	if err != nil {
		return nil, err
	}
	mods, err := modulePaths(root)
	if err != nil {
		return nil, err
	}
	rows := Rows(ledger)
	tips := map[string]map[string]bool{}
	seen := map[key]bool{}
	var out []Vanished
	for _, f := range files {
		src, ok := srcs[f]
		if !ok {
			continue
		}
		fns, err := testFuncs(f, src)
		if err != nil {
			return nil, fmt.Errorf("%s at %s: %v", f, base, err)
		}
		dir := path.Dir(f)
		if tips[dir] == nil {
			if tips[dir], err = tipTests(root, dir); err != nil {
				return nil, err
			}
		}
		for name, line := range fns {
			k := key{dir, name}
			if tips[dir][name] || seen[k] {
				continue
			}
			seen[k] = true
			pkg := importPath(mods, dir)
			if rows[[2]string{pkg, name}] {
				continue
			}
			tags := "none"
			if m := buildRe.FindSubmatch(src); m != nil {
				tags = strings.TrimSpace(string(m[1]))
			}
			out = append(out, Vanished{dir, pkg, name, f, line, tags})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pkg != out[j].Pkg {
			return out[i].Pkg < out[j].Pkg
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Skeleton is the ledger row v still needs, in the ledger's row shape.
func (v Vanished) Skeleton(base string) string {
	short := base
	if len(short) > 8 {
		short = short[:8]
	}
	return fmt.Sprintf("### <CODE> — `%s`\n"+
		"- test: `%s` · pkg `%s` · run `-run '^%s$'` · `%s:%d` @ `%s` · tags `%s`\n"+
		"- code: <CODE>\n"+
		"- assertion map: \n"+
		"- mutation: \n"+
		"- coverage Δ: \n"+
		"- citations rewritten: \n"+
		"- verified at: `<sha>`\n",
		v.Name, v.Name, v.Pkg, v.Name, v.File, v.Line, short, v.Tags)
}

func main() {
	root := flag.String("root", ".", "repo root holding the five modules")
	baseRef := flag.String("base", os.Getenv("TEST_LEDGER_BASE"), "base ref (default: TEST_LEDGER_BASE, else merge-base HEAD origin/main)")
	ledgerPath := flag.String("ledger", "", "ledger file (default: "+LedgerPath+" under -root)")
	flag.Parse()
	if *ledgerPath == "" {
		*ledgerPath = filepath.Join(*root, filepath.FromSlash(LedgerPath))
	}
	base, err := ResolveBase(*root, *baseRef)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testledger:", err)
		os.Exit(2)
	}
	ledger, err := os.ReadFile(*ledgerPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "testledger:", err)
		os.Exit(2)
	}
	gone, err := Check(*root, base, ledger)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testledger:", err)
		os.Exit(2)
	}
	if len(gone) == 0 {
		return
	}
	fmt.Printf("%d test(s) present at %.8s are gone at the tip with no row in %s:\n\n", len(gone), base, LedgerPath)
	for _, v := range gone {
		fmt.Println(v.Skeleton(base))
	}
	os.Exit(1)
}
