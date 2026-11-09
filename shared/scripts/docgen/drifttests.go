package main

// drifttests.go — the drift-test inventory in
// docs/ui-style-guide/24-drift-detection.md: every Test function in every
// `*_drift_test.go` file of the workspace's modules, with the first sentence
// of its doc comment. Sorted by file path, tests in file order.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

func init() {
	register(Generator{Name: "*_drift_test.go", Emit: func(root string, args []string) (string, error) {
		if len(args) != 0 {
			return "", fmt.Errorf("takes no arguments, got %q", args)
		}
		return drifttestsTable(root)
	}})
}

// drifttestsRow is one drift test.
type drifttestsRow struct{ file, test, doc string }

// drifttestsFind walks each workspace module for *_drift_test.go files and
// reads their top-level Test functions.
func drifttestsFind(root string) ([]drifttestsRow, error) {
	tree, err := newGosrcTree(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, d := range tree.modules {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	var files []string
	for _, d := range dirs {
		base := filepath.Join(root, filepath.FromSlash(d))
		err := filepath.WalkDir(base, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			n := e.Name()
			if e.IsDir() {
				if p != base && (strings.HasPrefix(n, ".") || n == "vendor" || n == "testdata" || n == "node_modules") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(n, "_drift_test.go") {
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	fset := token.NewFileSet()
	var rows []drifttestsRow
	for _, f := range files {
		af, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(f)), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !drifttestsIsTest(fd) {
				continue
			}
			rows = append(rows, drifttestsRow{f, fd.Name.Name, gosrcDocSentence(fd.Doc)})
		}
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no *_drift_test.go file in the workspace")
	}
	return rows, nil
}

// drifttestsIsTest reports whether fd is a test: a func Test... taking one *testing.T.
func drifttestsIsTest(fd *ast.FuncDecl) bool {
	name := fd.Name.Name
	if !strings.HasPrefix(name, "Test") || name == "TestMain" {
		return false
	}
	if rest := strings.TrimPrefix(name, "Test"); rest != "" && !(rest[0] < 'a' || rest[0] > 'z') {
		return false
	}
	p := fd.Type.Params.List
	return len(p) == 1 && gosrcExpr(p[0].Type) == "*testing.T"
}

func drifttestsTable(root string) (string, error) {
	rows, err := drifttestsFind(root)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("| File | Test | What it pins |\n|---|---|---|\n")
	for _, r := range rows {
		doc := r.doc
		if doc == "" {
			doc = "—"
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %s |\n", r.file, r.test, gosrcCell(doc))
	}
	return b.String(), nil
}
