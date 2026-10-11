package lockcheck

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var declaresALock = regexp.MustCompile(`lockcheck\.(RW)?Mutex\[`)

// TestEveryFileThatDeclaresALockImportsSync holds the production build to the
// plain lock (LOCKS.md §6). Mutex[R] is an alias of sync.Mutex there, but Go
// inlines the lock's fast path into a package only when it has sync's inline
// bodies, which a package that never imports sync may not: cms/poster, the
// one such package, called sync.(*Mutex).Lock where the plain build inlined
// its compare-and-swap. So every file that declares one imports sync, blank
// when nothing else there uses it.
func TestEveryFileThatDeclaresALockImportsSync(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	checked := 0
	for _, mod := range []string{"shingo-core", "shingo-edge", "protocol"} {
		err := filepath.WalkDir(filepath.Join(root, mod), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
				filepath.Base(filepath.Dir(path)) == "lockcheck" {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly|parser.ParseComments)
			if err != nil {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !declaresALock.Match(stripComments(src)) {
				return nil
			}
			checked++
			for _, im := range f.Imports {
				if im.Path.Value == `"sync"` {
					return nil
				}
			}
			t.Errorf("%s declares a lockcheck lock and does not import sync", path)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 50 {
		t.Fatalf("checked %d files that declare a lock; the walk missed the tree", checked)
	}
}

func stripComments(src []byte) []byte {
	return regexp.MustCompile(`//[^\n]*`).ReplaceAll(src, nil)
}
