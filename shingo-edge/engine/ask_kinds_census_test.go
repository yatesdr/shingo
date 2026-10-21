package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/internal/askbridge"
)

// TestAskKinds_EveryKindIsSentAndDeclared is the Edge's half of the ask
// kinds' census (protocol/ask.go: "pinned by each side's census test"):
//   - every kind in protocol.AskKinds has a sender in the Edge's non-test
//     source: a call whose function is an ask (Ask, AskLate, askOrRefuse,
//     askLateOrRefuse, ...) naming the kind's constant;
//   - every kind has a declared entry in askAppliersByKind, an applier or nil;
//   - the test bridges' table (internal/askbridge) names kinds Core answers.
//
// A kind added to protocol with no Edge sender, or sent with no decision about
// what its answer applies, fails here.
func TestAskKinds_EveryKindIsSentAndDeclared(t *testing.T) {
	t.Parallel()
	consts := askKindConsts(t)
	sent := askKindsSentByTheEdge(t, consts)

	appliers := askAppliersByKind()
	kinds := map[string]bool{}
	for _, k := range protocol.AskKinds() {
		kinds[k] = true
		if !sent[k] {
			t.Errorf("ask kind %q has no sender in the Edge's non-test source", k)
		}
		if _, ok := appliers[k]; !ok {
			t.Errorf("ask kind %q has no entry in askAppliersByKind (an applier, or nil for none)", k)
		}
	}
	for k := range appliers {
		if !kinds[k] {
			t.Errorf("askAppliersByKind names %q, which is not in protocol.AskKinds", k)
		}
	}
	for k := range askbridge.Paths {
		if !kinds[k] {
			t.Errorf("the test bridge names %q, which is not in protocol.AskKinds", k)
		}
	}
}

// askKindConsts maps each Ask* constant of protocol/ask.go to its value.
func askKindConsts(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "protocol", "ask.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse protocol/ask.go: %v", err)
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Ask") || i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					out[name.Name] = strings.Trim(lit.Value, "\"`")
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no Ask* constants found in protocol/ask.go")
	}
	return out
}

// askKindsSentByTheEdge is the kinds named as an argument of an ask call in
// shingo-edge's non-test Go source.
func askKindsSentByTheEdge(t *testing.T, consts map[string]string) map[string]bool {
	t.Helper()
	sent := map[string]bool{}
	var files []string
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) && path != ".." {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk shingo-edge: %v", err)
	}
	sort.Strings(files)
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !strings.Contains(strings.ToLower(calleeName(call.Fun)), "ask") {
				return true
			}
			for _, arg := range call.Args {
				if sel, ok := arg.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "protocol" {
						if v, ok := consts[sel.Sel.Name]; ok {
							sent[v] = true
						}
					}
				}
			}
			return true
		})
	}
	return sent
}

// calleeName is a call's function name: the identifier, the selector's field,
// or the instantiated generic's base.
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	case *ast.IndexExpr:
		return calleeName(f.X)
	case *ast.IndexListExpr:
		return calleeName(f.X)
	}
	return ""
}
