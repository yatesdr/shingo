package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestMigrationVerifyPredicatesAssertAbsenceWithTheAbsenceHelpers is the lint
// behind the ColumnAbsent / TableAbsent / IndexAbsent family.
//
// A verify predicate answers "does this migration's post-condition hold". The
// Exists helpers return false on a query error, which is the right direction
// when false means "not applied, re-run an idempotent statement". NEGATED, that
// direction inverts: a check that could not run reports the post-condition as
// HOLDING, and the migration is recorded as applied without anything having
// been checked. The absence helpers return false on error instead, so a failed
// check re-runs an idempotent DROP and costs nothing.
//
// SCOPED TO VERIFY PREDICATES ON PURPOSE. The same `!schema.ColumnExists(...)`
// spelling inside a migration BODY is a guard on whether to do the work, and
// there the existing direction is the safe one — on error it attempts the
// idempotent DDL rather than skipping it. Swapping those to the absence helpers
// would invert a correct check. The lint keys on the parameter type, which is
// what actually distinguishes the two.
func TestMigrationVerifyPredicatesAssertAbsenceWithTheAbsenceHelpers(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "migrations.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse migrations.go: %v", err)
	}

	banned := map[string]string{
		"ColumnExists": "schema.ColumnAbsent",
		"TableExists":  "schema.TableAbsent",
		"IndexExists":  "schema.IndexAbsent",
	}

	var findings []string
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok || !takesQuerier(lit.Type) {
			return true
		}
		ast.Inspect(lit.Body, func(inner ast.Node) bool {
			unary, ok := inner.(*ast.UnaryExpr)
			if !ok || unary.Op != token.NOT {
				return true
			}
			call, ok := unary.X.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "schema" {
				return true
			}
			if want, bad := banned[sel.Sel.Name]; bad {
				findings = append(findings, "migrations.go:"+
					ffPos(fset, unary.Pos())+" !schema."+sel.Sel.Name+"(...) — use "+want+"(...)")
			}
			return true
		})
		return true
	})

	if len(findings) > 0 {
		t.Errorf("%d verify predicate(s) assert an absence by negating an Exists helper. "+
			"On a query failure those report the post-condition as holding, so the migration "+
			"is recorded as applied without having checked anything:\n  %s",
			len(findings), strings.Join(findings, "\n  "))
	}
}

// takesQuerier reports whether a func literal is a migration verify predicate,
// i.e. its single parameter is a schema.Querier.
func takesQuerier(ft *ast.FuncType) bool {
	if ft.Params == nil || len(ft.Params.List) != 1 {
		return false
	}
	sel, ok := ft.Params.List[0].Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "schema" && sel.Sel.Name == "Querier"
}

func ffPos(fset *token.FileSet, p token.Pos) string {
	return strings.TrimPrefix(fset.Position(p).String(), "migrations.go:")
}
