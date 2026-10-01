package release

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestReleaseIsPure: the release package imports the protocol and the
// standard library, nothing else — no store, no orders, no messaging, no PLC,
// no Core client. So a gate cannot read, write or block: every fact it decides
// on arrives in the snapshot, and every effect is the commit's.
func TestReleaseIsPure(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Clean(name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: import %s: %v", name, imp.Path.Value, err)
			}
			first, _, _ := strings.Cut(path, "/")
			stdlib := !strings.Contains(first, ".") && first != "shingo" && first != "shingoedge"
			if stdlib || path == "shingo/protocol" {
				continue
			}
			t.Errorf("%s imports %q: the release package may import only shingo/protocol and the standard library", name, path)
		}
	}
	if checked == 0 {
		t.Fatal("no source files checked")
	}
}
