package helpers_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// siblingDepthComparison matches a depth comparison against another node's depth
// INSIDE A SQL STRING — the signature of the sibling scan. It deliberately does
// not match Go-level depth comparisons (binresolver's candidateBetter,
// dispatch/lane_entry's classifyLaneEntry, the lane gate's release-ordering
// sorts), because those are ranking and admission questions, not this predicate:
// the scan below only looks at string literals, so Go expressions never reach it.
var siblingDepthComparison = regexp.MustCompile(`\.depth\s*[<>]|[<>]\s*[\w.%\[\]]*\.depth`)

// exemptDirs are the two parallel implementations that must NOT be migrated onto
// the shared predicate, so the guard has to know not to count them.
//
//   - scenesim is a reimplementation on purpose and has already diverged in ways
//     that are correct for it: its lane index is dense and 0-based where
//     production's depth is sparse and 1-based, and its real predicate starts
//     from the ROBOT'S CURRENT POSITION rather than the mouth, so that a bin
//     behind an already-entered robot is not treated as a wall. That start
//     offset has no production analogue and must not acquire one.
//   - plantspec is the authoring contract (depth >= 1), not the runtime
//     predicate.
var exemptDirs = []string{
	"fleet/simulator/scenesim",
	"plantspec",
}

// TestReachabilityHasExactlyOneSpelling is the point of the whole exercise, made
// mechanical.
//
// "A slot is reachable iff no occupied slot sits strictly shallower in the same
// lane" was written seven times — a Go loop, a COUNT, four correlated
// sub-queries, and a sort key — and the copies disagreed about NULL depths, about
// whether to scope siblings by the lane parameter or by correlation, and about
// what a failed read means. None of that was visible at any one call site.
//
// So the single definition is enforced rather than asked for. A future inline
// copy of the sibling scan fails HERE, in a test naming the reason, instead of
// drifting quietly for another year.
//
// If this fires on a legitimately new use, the fix is to interpolate
// helpers.ReachableSQL / helpers.BuriedSQL, not to widen the exemption list.
func TestReachabilityHasExactlyOneSpelling(t *testing.T) {
	t.Parallel()
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}

	var found []string
	fset := token.NewFileSet()

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		slashed := filepath.ToSlash(rel)
		if d.IsDir() {
			if slices.Contains(exemptDirs, slashed) {
				return fs.SkipDir
			}
			return nil
		}
		// Tests write whatever SQL they need to pin behaviour; the guard is about
		// production spellings.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // unparseable file is someone else's test to fail
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				val = lit.Value // raw backtick strings unquote fine; be lenient anyway
			}
			if siblingDepthComparison.MatchString(val) {
				found = append(found, slashed+":"+strconv.Itoa(fset.Position(lit.Pos()).Line))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	const home = "store/internal/helpers/lane_reachability.go"
	if len(found) != 1 || !strings.HasPrefix(found[0], home+":") {
		t.Fatalf("the sibling-depth comparison must appear in exactly one place (%s, inside "+
			"laneBlockerPredicate); found %d:\n  %s\n\n"+
			"Every occurrence beyond the first is a second definition of reachability. The seven that existed "+
			"before disagreed about NULL depths, about correlating the sibling scope versus keying it on the lane "+
			"parameter, and about what a failed read means — and none of that was visible from any one call site. "+
			"Interpolate helpers.ReachableSQL or helpers.BuriedSQL instead of writing the scan again.",
			home, len(found), strings.Join(found, "\n  "))
	}
}

// moduleRoot walks up from the test's working directory (the package dir) until
// it finds go.mod, rather than counting ".." hops — the guard has already moved
// package once, and a relative-depth walk would have silently started scanning
// the wrong subtree instead of failing.
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
