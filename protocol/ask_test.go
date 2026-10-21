package protocol

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestAskKinds_NamesEveryAskConst: AskKinds lists every Ask* kind constant in
// ask.go exactly once, so a kind cannot be declared without joining the list
// each side's census fences.
func TestAskKinds_NamesEveryAskConst(t *testing.T) {
	t.Parallel()
	f, err := parser.ParseFile(token.NewFileSet(), "ask.go", nil, 0)
	if err != nil {
		t.Fatalf("parse ask.go: %v", err)
	}
	listed := map[string]int{}
	for _, k := range AskKinds() {
		listed[k]++
	}
	declared := 0
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, s := range g.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if !strings.HasPrefix(n.Name, "Ask") || n.Name == "AskTTL" || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					continue
				}
				declared++
				if v := strings.Trim(lit.Value, "\""); listed[v] != 1 {
					t.Errorf("%s (%s) is listed %d times in AskKinds, want once", n.Name, v, listed[v])
				}
			}
		}
	}
	if declared != len(AskKinds()) {
		t.Errorf("ask.go declares %d kinds, AskKinds lists %d", declared, len(AskKinds()))
	}
}

// TestAskSubjects_TTLs: an ask expires after AskTTL; an answer never does.
func TestAskSubjects_TTLs(t *testing.T) {
	t.Parallel()
	if got := DataTTLFor(SubjectEdgeAsk); got != AskTTL {
		t.Errorf("edge.ask TTL = %v, want %v", got, AskTTL)
	}
	if got := DataTTLFor(SubjectEdgeAnswer); got != NoExpiry {
		t.Errorf("edge.answer TTL = %v, want NoExpiry", got)
	}
}
