package www

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestLoaderDoors_ReachNoDispatcher: the loader doors (handlers_loader.go)
// save through the loader service and, where a save drains, through the
// engine's one verb for it (EngineOrchestration.SaveLoader). None calls the
// dispatcher: a door that saved and then drained by hand was the decision
// wired in www, the shape the verb exists to end.
func TestLoaderDoors_ReachNoDispatcher(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "handlers_loader.go", nil, 0)
	if err != nil {
		t.Fatalf("parse the loader doors: %v", err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Dispatcher" {
			t.Errorf("%s: a loader door reaches the dispatcher", fset.Position(sel.Pos()))
		}
		return true
	})
}
