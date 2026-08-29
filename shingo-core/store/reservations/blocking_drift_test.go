package reservations_test

import (
	"go/ast"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// blockingHome is the package allowed to spell a reservation's state predicate.
// A package rather than a single file, because the mouth family (mouth.go) and
// the lifecycle primitives (reservations.go) are legitimately the writers of
// their own rows and read their own states — the drift this guards against is a
// QUERY IN ANOTHER PACKAGE deciding for itself what "spoken for" means.
const blockingHome = "shingo-core/store/reservations/"

// migrationsAreTheDefinition is the one exemption, and it is not a loophole.
//
// store/migrations.go carries `state IN ('pending','confirmed')` as the
// PREDICATE OF THE PARTIAL UNIQUE INDEXES (v43, v44). That is not a copy of the
// Go fragment, it is the thing the Go fragment transcribes — and a migration
// that has shipped is immutable by house rule, so it could not be rewritten to
// call a renderer even if that were desirable. When the narrowing changes what
// blocks, it changes BOTH, in one migration, deliberately.
const migrationsAreTheDefinition = "shingo-core/store/migrations.go"

// stateSpelling matches a reservations state predicate written into a SQL
// string: `state = 'pending'`, `state='pending'`, or
// `state IN ('pending','confirmed')`, with or without a table alias.
var stateSpelling = regexp.MustCompile(`state\s*(=\s*'pending'|IN\s*\(\s*'pending')`)

// TestBlockingPredicateHasExactlyOneSpelling is dig_exclusion's guard applied to
// the question underneath it: does a reservation row on this resource make the
// resource unavailable?
//
// That question was written out by hand at twenty-three queries in eight
// packages. They agreed — which is the dangerous case, because agreement by
// coincidence looks exactly like agreement by design right up until somebody
// edits twenty-two of them. The reservation tier changes the answer (a
// reservation is Core sourcing ahead of the call, and must not hide a bin from
// the demand being called for), so the twenty-three copies were about to become
// twenty-three chances to get one wrong.
//
// EXPECTED COUNT IS ZERO OUTSIDE THE PACKAGE, which is the same standard the
// dig exclusion holds itself to. A match means somebody wrote the state
// predicate at a query instead of composing reservations.BinSpokenForSQL,
// HeldByOwnerSQL, SlotSpokenForByStrangerSQL, OnTheBooksSQL, or — for a fragment
// this file does not shape, like a lane cordon or a presence witness —
// ActiveStateSQL / BlockingStateSQL.
func TestBlockingPredicateHasExactlyOneSpelling(t *testing.T) {
	t.Parallel()
	var found []string
	walkProduction(t, func(rel string, file *ast.File, fset *token.FileSet) {
		slash := "shingo-core/" + rel
		if strings.HasPrefix(slash, blockingHome) || slash == migrationsAreTheDefinition {
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
			if stateSpelling.MatchString(val) {
				found = append(found, rel+":"+strconv.Itoa(fset.Position(lit.Pos()).Line))
			}
			return true
		})
	})
	if len(found) != 0 {
		t.Fatalf("a reservation's state predicate must not be spelled at a query; found %d:\n  %s\n\n"+
			"Compose the fragment from %s instead. Every hand-written copy is a second definition "+
			"of what a reservation blocks, and there were twenty-three of them — which is twenty-three "+
			"edits, in lockstep, the day a reservation stops hiding a bin from the demand it was "+
			"sourced for.",
			len(found), strings.Join(found, "\n  "), blockingHome)
	}
}
