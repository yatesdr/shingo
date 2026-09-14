package audit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cycle_time_test.go — the drift guards for the cycle-time read.
//
// Neither test needs a database, deliberately: both hold a claim about ANOTHER
// FILE's source, which is where the cycle-time surface's two silent-failure
// modes live. A query that filters on a stale op string returns zero rows and
// renders as an idle plant; a query grained on a column the writer has started
// populating renders one nameless key forever.

// applierSource reads the file that writes the rows this package reads.
func applierSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "uop", "applier.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (these tests hold this package's claims about what the "+
			"applier writes; if the file moved, repoint them rather than deleting them)",
			path, err)
	}
	return string(b)
}

// TestCycleOpMatchesTheApplier.
//
// OpBinUOPDelta is a SECOND SPELLING of a string that already exists — the
// applier writes 'bin_uop_delta' as a literal inside its INSERT and references
// no constant. Two spellings of one string is a drift waiting to happen, and the
// drift is invisible: the query returns zero rows and the page renders its empty
// state, which says "no cycles", which is the reassuring reading.
//
// The honest way to hold two spellings together is to check them.
//
// VERIFIED RED BY: changing OpBinUOPDelta to "bin_uop_deltas" — the test named
// both the constant and the file it disagrees with.
func TestCycleOpMatchesTheApplier(t *testing.T) {
	src := applierSource(t)
	if !strings.Contains(src, "'"+OpBinUOPDelta+"'") {
		t.Errorf("uop/applier.go does not write '%s' as an op literal.\n"+
			"OpBinUOPDelta is a restatement of the applier's literal, and if the two "+
			"disagree ListCycleEvents returns no rows and the cycle-time page renders its "+
			"empty state — which reads as an idle plant rather than as a broken filter.",
			OpBinUOPDelta)
	}
}

// TestCycleSurfaceAwaitsTheNodeRegrain pins the two halves of a grain that is
// mid-move: the applier now records the node, and this surface has not yet
// followed it.
//
// THE STYLE GUIDE ASSIGNS 5.10 A DISTRIBUTION PER (NODE, PAYLOAD). Until the
// node stamp landed it could not be built at all — the applied-delta INSERT
// recorded neither node_id nor station, so the node was absent from the truth
// path and no join could put it back (a bin's node is where it is NOW, not
// where it was when the tick landed). The INSERT now stamps node_id from the
// bins row its transaction already holds, so the grain has become reachable.
//
// It is not yet usable, and that is why this test guards rather than
// celebrates. Every row written before the stamp carries node_id NULL, and
// there is no honest backfill for them. A surface re-grained onto the column
// today would render one nameless key for its whole history and a few real
// nodes at the tail — worse than the station grain it has now. The re-grain
// waits on the column accumulating, not on anybody's permission.
//
// So the assertions below are a ratchet in both directions: the stamp must not
// regress (that would strand the re-grain indefinitely, and nothing else
// watches it), and the station must keep arriving in the actor slot
// ListCycleEvents actually reads for as long as the surface is grained on it.
//
// VERIFIED RED BY: dropping node_id from the applier's INSERT column list (the
// first half), and by moving station out of the actor position (the second).
func TestCycleSurfaceAwaitsTheNodeRegrain(t *testing.T) {
	src := applierSource(t)

	// The INSERT that writes an APPLIED delta. Located by its op literal so this
	// does not match the observation-row inserts (stale-epoch, payload-mismatch),
	// which are a different shape and are not cycles.
	re := regexp.MustCompile(`(?s)INSERT INTO bin_uop_ledger\s*\n?\s*\(([^)]*)\)[^;]*?'` + OpBinUOPDelta + `'`)
	loc := re.FindStringSubmatchIndex(src)
	if loc == nil {
		t.Fatalf("could not locate the applied-delta INSERT in uop/applier.go — the pattern " +
			"is stale, and a silent miss here would leave this guard passing on nothing " +
			"while the grain question went unwatched")
	}
	cols := src[loc[2]:loc[3]]

	if !regexp.MustCompile(`\bnode_id\b`).MatchString(cols) {
		t.Errorf("the applied-delta INSERT no longer writes node_id.\n\n"+
			"That column is the only record of WHERE a count changed, and it cannot be "+
			"recovered afterwards. Dropping it does not postpone the (node, payload) "+
			"grain, it forecloses it for every row written while the stamp is gone.\n\n"+
			"Columns written: %s", strings.Join(strings.Fields(cols), " "))
	}

	// And the half that is still load-bearing today: the station really is
	// arriving as the actor, which is the column ListCycleEvents reads. Without
	// this, a present node_id would be consistent with a surface that had
	// quietly lost the grain it is actually still serving.
	//
	// SCOPED TO THIS CALL'S OWN ARGUMENTS, which the predecessor of this test
	// was not. It matched the token pair against the whole file behind a comment
	// claiming it pinned the actor slot "specifically" — but `d.PayloadCode,
	// station,` appears at seven other call sites in applier.go, so moving the
	// station out of the actor position here left it green. Narrowing to the
	// window between this INSERT's op literal and its `); err != nil` is what
	// makes the claim true: the pair has to be in THIS argument list. The token
	// is `station` and not `d.Station` because the station moved off the delta
	// payload onto the envelope — it is the applier's first argument now.
	argsEnd := len(src)
	if n := strings.Index(src[loc[1]:], "); err != nil"); n >= 0 {
		argsEnd = loc[1] + n
	}
	if !strings.Contains(src[loc[1]:argsEnd], "d.PayloadCode, station,") {
		t.Error("the applier no longer passes the envelope station into the audit INSERT's " +
			"actor position — ListCycleEvents reads the actor column for the station and " +
			"would now return one nameless key for the whole site")
	}
}
