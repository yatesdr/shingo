package protocol_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The wire vocabulary — the strings Core and Edge must spell identically
// because they cross the wire — is defined once, in protocol/. These guards
// keep it that way.
//
// It was NOT that way. The eight loader values (produce/consume,
// shared_window/dedicated_positions, operator/threshold, home/buffer) were
// written out independently in shingo-core/store/loaders and
// shingo-edge/domain, the two inventory-delta scope kinds in shingo-core/uop
// and shingo-edge/uop, and the outbox retry cap in three places. They all
// agreed on 2026-08-19, which is the point: nothing made them agree, so the
// agreement was a coincidence that held. A rename on one side alone changes a
// loader's behavior at one plant, or silently stops inventory-delta
// deduplication, and no compiler anywhere would say so.
//
// Modelled on store/reservations/dig_exclusion_drift_test.go, which exists for
// the same failure — a question with three answers in three subsystems and no
// call site able to see the disagreement.

// vocabularyNames are the constant names that carry wire vocabulary. Outside
// protocol these may be DERIVED from a protocol constant but must not be bound
// to a string literal — that is a second definition wearing the same name.
var vocabularyNames = map[string]bool{
	"RoleProduce":              true,
	"RoleConsume":              true,
	"LayoutSharedWindow":       true,
	"LayoutDedicatedPositions": true,
	"ReplenishmentOperator":    true,
	"ReplenishmentThreshold":   true,
	"HomeKindHome":             true,
	"HomeKindBuffer":           true,
	"invDeltaScopeBin":         true,
	"invDeltaScopeBucket":      true,
}

// distinctiveValues are vocabulary strings specific enough that a constant
// bound to one outside protocol is drift regardless of what it is called.
//
// Only the compound ones. "operator", "home", "buffer" and "bin" are ordinary
// words this repo legitimately uses for unrelated vocabularies — a reservation
// resource_kind of "bin", an audit actor of "operator" — and a guard that
// fired on those would be turned off within a week, which is worse than no
// guard. The name check above covers the single-word values.
var distinctiveValues = map[string]bool{
	"shared_window":       true,
	"dedicated_positions": true,
}

func TestWireVocabularyHasOneDefinitionSite(t *testing.T) {
	t.Parallel()

	var byName, byValue []string
	walkRepoProduction(t, func(rel string, file *ast.File, fset *token.FileSet) {
		if strings.HasPrefix(rel, "protocol/") {
			return // the home
		}
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range spec.Names {
				if i >= len(spec.Values) {
					continue
				}
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue // derived from protocol, which is the point
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				at := rel + ":" + strconv.Itoa(fset.Position(spec.Pos()).Line) + " " + name.Name
				if vocabularyNames[name.Name] {
					byName = append(byName, at+" = "+lit.Value)
				} else if distinctiveValues[val] {
					byValue = append(byValue, at+" = "+lit.Value)
				}
			}
			return true
		})
	})

	if len(byName) != 0 {
		t.Errorf("wire-vocabulary constant(s) bound to a string literal outside protocol/, in %d place(s):\n  %s\n\n"+
			"Derive them from the protocol constant instead (e.g. LayoutSharedWindow = protocol.LoaderLayoutSharedWindow). "+
			"A literal here is a second definition of a value the other module also spells, and the two agreeing is "+
			"then a coincidence rather than a property.",
			len(byName), strings.Join(byName, "\n  "))
	}
	if len(byValue) != 0 {
		t.Errorf("loader-layout value(s) spelled outside protocol/, in %d place(s):\n  %s\n\n"+
			"These strings cross the wire; protocol/ owns them.",
			len(byValue), strings.Join(byValue, "\n  "))
	}
}

// TestOutboxRetryCapHasOneDefinitionSite is the same guard for the retry cap,
// which was an integer rather than a string and so needs its own scan. It was
// defined three times — protocol/outbox, shingo-core/store/messaging and
// shingo-edge/store/messaging — for one number that the drainer enforces.
func TestOutboxRetryCapHasOneDefinitionSite(t *testing.T) {
	t.Parallel()

	var found []string
	walkRepoProduction(t, func(rel string, file *ast.File, fset *token.FileSet) {
		if rel == "protocol/outbox/drainer.go" {
			return // the home
		}
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range spec.Names {
				if i >= len(spec.Values) {
					continue
				}
				if name.Name != "MaxRetries" && name.Name != "MaxOutboxRetries" {
					continue
				}
				if lit, ok := spec.Values[i].(*ast.BasicLit); ok && lit.Kind == token.INT {
					found = append(found, rel+":"+strconv.Itoa(fset.Position(spec.Pos()).Line)+" "+name.Name+" = "+lit.Value)
				}
			}
			return true
		})
	})

	if len(found) != 0 {
		t.Fatalf("the outbox retry cap is written as a literal in %d place(s):\n  %s\n\n"+
			"Use outbox.MaxRetries. The drainer that enforces the cap lives in protocol/outbox, so "+
			"a store-side copy that drifts below it dead-letters rows the drainer would still retry, "+
			"and one above it hides rows the drainer has already given up on.",
			len(found), strings.Join(found, "\n  "))
	}
}

// walkRepoProduction visits every non-test .go file in EVERY module. The
// vocabulary's whole problem is that it spans modules, so a single-module walk
// — which is what the dig-exclusion guard this is modelled on needs — would be
// blind to exactly the drift being guarded.
func walkRepoProduction(t *testing.T, fn func(rel string, file *ast.File, fset *token.FileSet)) {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	fset := token.NewFileSet()
	seen := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return fs.SkipDir
			}
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
			return nil // an unparseable file is someone else's test to fail
		}
		seen++
		fn(filepath.ToSlash(rel), file, fset)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// A walk that finds nothing passes every assertion above, silently.
	if seen < 100 {
		t.Fatalf("walked only %d production files under %s — the walk is broken, not the repo clean", seen, root)
	}
}

// repoRoot walks up to the directory holding go.work, which is the only marker
// that means "all the modules". moduleRoot-style go.mod detection would stop at
// protocol/ and see one fifth of the repo.
func repoRoot() (string, error) {
	dir, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
