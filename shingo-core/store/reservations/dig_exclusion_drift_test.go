package reservations_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// home is the one file allowed to spell the dig-exclusion comparison.
const home = "store/reservations/dig_exclusion.go"

// sqlOwnerExclusion matches an order-id NOT-EQUALS against a bind parameter
// inside a SQL string — the signature of somebody writing the dig exclusion out
// by hand instead of interpolating DigExclusionSQL.
//
// Not-equals only, deliberately. `order_id = $1` is the ordinary owner-scoped
// read (ReleaseLane, ReleaseLanesByOwner, the mouth upsert) and appears all over
// this package legitimately. It is the NEGATION that is this predicate: "every
// dig hold except the asker's".
var sqlOwnerExclusion = regexp.MustCompile(`order_id\s*(<>|!=)\s*\$`)

// mouthContext narrows the guard to the DIG lock. Without it this test fires on
// two other owner-exemptions, and both are correct where they are:
//
//   - nodes.FindStoreSlotInLaneExcluding — resource_kind='slot'. The precedent
//     this whole change was modelled on, including the sentinel: its doc already
//     records that "excludeOrderID = 0 disables the exemption and reproduces the
//     blind behavior exactly", which is what reservations.Anyone is.
//   - orders.stealSoftHold — resource_kind='bin', and a THREE-arm exemption
//     (self, parent, and siblings via parent_order_id).
//
// So the ownership rule is written three times over three resource kinds, with
// one, two and three arms respectively. That is a real observation and it is NOT
// this test's business: they exclude different FACTS, and collapsing them would
// mean deciding that a slot hold, a bin hold and a lane dig keep out the same
// set — which is a design question with a plant behind it, not a refactor. This
// guard keeps the dig question singular. Widening it to the others needs that
// question answered first.
var mouthContext = regexp.MustCompile(`'mouth'`)

// TestDigExclusionHasExactlyOneSQLSpelling is the reachability guard's twin
// (store/internal/helpers/lane_reachability_drift_test.go), for the same reason
// and against the same failure mode.
//
// The question "does the dig holding this lane exclude the order asking" had
// three answers in three subsystems. Admission exempted the asker and its
// compound parent; the sourcing scan exempted nobody; dig planning never asked.
// No single call site could see the disagreement, and it arrested the plant:
// an expose dig's own lock hid the bin it had just uncovered from the parent
// the dig was run for.
//
// EXPECTED COUNT IS ZERO, WHICH IS STRONGER THAN ONE. The surviving spelling
// lives in a format string ("%s <> $%d AND %s <> $%d") that names no column, so
// the literal below should appear nowhere in production. A match means someone
// wrote the comparison at a query instead of interpolating the renderer, which
// is exactly how the three spellings arose the first time.
func TestDigExclusionHasExactlyOneSQLSpelling(t *testing.T) {
	t.Parallel()
	var found []string
	walkProduction(t, func(rel string, file *ast.File, fset *token.FileSet) {
		if rel == home {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil {
				val = lit.Value
			}
			if sqlOwnerExclusion.MatchString(val) && mouthContext.MatchString(val) {
				found = append(found, rel+":"+strconv.Itoa(fset.Position(lit.Pos()).Line))
			}
			return true
		})
	})
	if len(found) != 0 {
		t.Fatalf("the dig-exclusion comparison must not be spelled at a query; found %d:\n  %s\n\n"+
			"Interpolate reservations.DigExclusionSQL and bind DigAsker.Args() instead. Every "+
			"hand-written copy is a second definition of who a dig keeps out, and the three that "+
			"existed before disagreed about whether the dig's OWNER was kept out — which is what "+
			"stopped the ring on 2026-08-10. See %s.",
			len(found), strings.Join(found, "\n  "), home)
	}
}

// TestDigOwnerComparisonHasExactlyOneSpelling is the Go-side half.
//
// The SQL guard above cannot see dispatch.ownsDig, which for a year WAS the
// second spelling: two hand-written comparisons of a dig owner against an order
// id, sitting in another package, disagreeing with the query by omission. So
// this one scans expressions rather than strings.
//
// Comparisons against the zero literal are exempt and must stay exempt: "is
// there a dig on this lane at all" (LaneLock.IsLocked, LockedBy, and
// ExcludedBy's own early return) is a different question with no asker in it.
func TestDigOwnerComparisonHasExactlyOneSpelling(t *testing.T) {
	t.Parallel()
	var found []string
	walkProduction(t, func(rel string, file *ast.File, fset *token.FileSet) {
		if rel == home {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			be, ok := n.(*ast.BinaryExpr)
			if !ok || (be.Op != token.EQL && be.Op != token.NEQ) {
				return true
			}
			if !namesADigOwner(be.X) && !namesADigOwner(be.Y) {
				return true
			}
			if isZeroLiteral(be.X) || isZeroLiteral(be.Y) {
				return true // "is anything holding this lane" — no asker involved
			}
			found = append(found, rel+":"+strconv.Itoa(fset.Position(be.Pos()).Line))
			return true
		})
	})
	if len(found) != 0 {
		t.Fatalf("a dig owner is compared against something other than zero in %d place(s):\n  %s\n\n"+
			"That comparison is reservations.DigAsker.ExcludedBy and it lives in %s. dispatch.ownsDig "+
			"used to write it out here, which is how admission came to exempt the dig's owner while "+
			"the sourcing query did not. Build an asker and call ExcludedBy.",
			len(found), strings.Join(found, "\n  "), home)
	}
}

// namesADigOwner reports whether an expression is an identifier or field whose
// name says it holds a dig owner. Name-based on purpose: the guard is about the
// question being asked, and the name is what tells a reader which question it is.
func namesADigOwner(e ast.Expr) bool {
	var name string
	switch v := e.(type) {
	case *ast.Ident:
		name = v.Name
	case *ast.SelectorExpr:
		name = v.Sel.Name
	case *ast.CallExpr:
		if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
			name = sel.Sel.Name
		}
	}
	lower := strings.ToLower(name)
	return lower == "digowner" || lower == "lockedby" || lower == "dighouldowner" || lower == "digholdowner"
}

func isZeroLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "0"
}

// walkProduction parses every non-test .go file in the module and hands it to
// fn with its module-relative slash path.
func walkProduction(t *testing.T, fn func(rel string, file *ast.File, fset *token.FileSet)) {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // unparseable file is someone else's test to fail
		}
		fn(filepath.ToSlash(rel), file, fset)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// moduleRoot walks up until it finds go.mod, rather than counting ".." hops —
// the reachability guard learned that the hard way when its package moved.
func moduleRoot() (string, error) {
	dir, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
