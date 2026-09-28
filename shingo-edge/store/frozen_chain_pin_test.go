package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The FROZEN-CHAIN PIN. Edge's migration history is a self-probing chain
// with no version marker, adopted mid-life at baseline v1 by the
// protocol/migrate runner (see migrate()'s tail). Adoption must not change
// it: no reordering, no insertion, no deletion, no "just one small fix" to
// a step that predates the version table. Those steps still run on every
// startup of every database this build opens, so an edit here reaches
// plant floors directly with no version-table gate to stop it.
//
// This test parses migrations.go and pins three things:
//
//  1. the statement count of migrate()'s body — any edit to the frozen
//     chain that adds or removes a statement flips it;
//  2. the last frozen statement is the EnsureClaimQuarantine if-block —
//     its "LAST, AND THAT IS ITS ONLY ORDERING REQUIREMENT" comment is a
//     documented invariant, and the versioned runner must come after it;
//  3. the final statement is the runVersioned call — the runner sits at
//     the tail and nowhere else, with the versioned-migrations marker
//     comment between it and the frozen chain.
//
// If this test fails because you ADDED A VERSIONED MIGRATION: good, but a
// new migration belongs in edgeMigrations(), not here — migrate()'s body
// does not change. If it fails because you edited a frozen step: that is
// the edit this test exists to stop. Revert it, ship the change as a
// versioned migration with Version > edgeBaselineVersion instead.
//
// ONE KIND OF FROZEN-STEP EDIT IS SANCTIONED: a step that re-creates what a
// versioned migration drops is removed WITH the drop, in the same change.
// Otherwise the chain, which re-runs on every boot, undoes the migration on
// the next start. First use: 2026-09-28, the payload_catalog.cycle_seconds
// ALTER removed with Edge v3 (wantTotal 165 -> 164). Removal only: a
// re-creating step is never rewritten in place, and nothing else qualifies.
// The chain's retirement condition is unchanged: every deployed Edge
// reports schema version >= 1.
//
// The runtime half of the pin — that a fresh Open seeds exactly the
// baseline row and an aged database is adopted, not re-run — is
// versioned_migration_test.go's job.
func TestFrozenChainIsPinned(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("migrations.go"), nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse migrations.go: %v", err)
	}

	var mig *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "migrate" {
			mig = fd
			break
		}
	}
	if mig == nil {
		t.Fatalf("no migrate() FuncDecl found in migrations.go")
	}

	const wantTotal = 164 // statements in migrate(): frozen chain + the runVersioned tail
	if got := len(mig.Body.List); got != wantTotal {
		t.Errorf("migrate() has %d statements, want %d — the frozen chain's shape changed. "+
			"New schema changes go in edgeMigrations(), not here; frozen steps are never edited. "+
			"See this test's header before changing wantTotal.", got, wantTotal)
	}

	// Last frozen statement: the EnsureClaimQuarantine if-block.
	last := len(mig.Body.List) - 1
	ifStmt, ok := mig.Body.List[last-1].(*ast.IfStmt)
	if !ok {
		t.Fatalf("statement before the versioned tail is a %T, want the EnsureClaimQuarantine if-block",
			mig.Body.List[last-1])
	}
	var callsQuarantine bool
	ast.Inspect(ifStmt, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "EnsureClaimQuarantine" {
			callsQuarantine = true
		}
		return true
	})
	if !callsQuarantine {
		t.Errorf("the last frozen statement does not call EnsureClaimQuarantine — the chain's " +
			"documented LAST step (see its comment in migrate()) must stay last")
	}

	// Final statement: return db.runVersioned(edgeMigrations()).
	ret, ok := mig.Body.List[last].(*ast.ReturnStmt)
	if !ok {
		t.Fatalf("migrate()'s final statement is a %T, want the runVersioned return", mig.Body.List[last])
	}
	var callsRunVersioned bool
	ast.Inspect(ret, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "runVersioned" {
				callsRunVersioned = true
			}
		}
		return true
	})
	if !callsRunVersioned {
		t.Errorf("migrate()'s final statement does not call runVersioned — the versioned runner " +
			"sits at the tail and nowhere else")
	}

	// The marker comment sits between the frozen chain and the tail, so the
	// boundary is greppable and the runner cannot silently move above a
	// frozen step.
	marker := fset.Position(ifStmt.End())
	tail := fset.Position(ret.Pos())
	found := false
	for _, cg := range f.Comments {
		p := fset.Position(cg.Pos())
		if p.Line > marker.Line && p.Line < tail.Line {
			for _, c := range cg.List {
				if strings.Contains(c.Text, "versioned migrations (protocol/migrate)") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("no versioned-migrations marker comment between the frozen chain and the " +
			"runVersioned tail — keep the `── versioned migrations (protocol/migrate) ──` marker")
	}
}
