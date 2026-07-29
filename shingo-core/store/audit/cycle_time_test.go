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

// TestAppliedDeltaStillCarriesNoNode pins the reason this surface is grained on
// (station, payload, direction) rather than on (node, payload).
//
// THE STYLE GUIDE ASSIGNS 5.10 A DISTRIBUTION PER (NODE, PAYLOAD). It cannot be
// built: the applied-delta INSERT names
// (bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata) and
// passes the Edge station as ACTOR, so node_id is NULL and the station column is
// empty on every row this op has ever written.
//
// This test fails the day someone fixes that — which is the point. A page grained
// on the station while the writer has started recording the node would be
// throwing away the better grain and nothing would say so. The failure message
// is the handover note.
//
// VERIFIED RED BY: adding node_id to the applier's INSERT column list — the test
// fired and said to re-grain the surface.
func TestAppliedDeltaStillCarriesNoNode(t *testing.T) {
	src := applierSource(t)

	// The INSERT that writes an APPLIED delta. Located by its op literal so this
	// does not match the observation-row inserts (stale-epoch, payload-mismatch),
	// which are a different shape and are not cycles.
	re := regexp.MustCompile(`(?s)INSERT INTO bin_uop_audit\s*\n?\s*\(([^)]*)\)[^;]*?'` + OpBinUOPDelta + `'`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("could not locate the applied-delta INSERT in uop/applier.go — the pattern " +
			"is stale, and a silent miss here would leave this guard passing on nothing " +
			"while the grain question went unwatched")
	}
	cols := m[1]

	for _, col := range []string{"node_id", "station"} {
		if regexp.MustCompile(`\b` + col + `\b`).MatchString(cols) {
			t.Errorf("the applied-delta INSERT now writes %s.\n\n"+
				"THIS IS GOOD NEWS AND IT NEEDS FOLLOWING UP. The cycle-time surface (5.10) "+
				"is grained on (station, payload, direction) ONLY because node was not "+
				"recoverable from this row — the style guide assigns it a distribution per "+
				"(node, payload). Re-grain domain.CycleKey and ListCycleEvents onto the "+
				"column that is now populated, and delete this test.\n\n"+
				"Columns written: %s", col, strings.Join(strings.Fields(cols), " "))
		}
	}

	// And the positive half: the station really is arriving as the actor, which is
	// the column ListCycleEvents reads. Without this, "no node_id" would be
	// consistent with a query that reads nothing at all.
	//
	// THE TOKEN CHANGED FROM `d.Station` TO `station` AND THE ASSERTION IS
	// STRONGER FOR IT. The station is no longer a field on the delta payload —
	// it was carried twice in one envelope, once by the transport and once by
	// the sender, and the handler's `if station == "" { … }` reconciliation was
	// a rule with two possible answers. It is now the applier's first argument,
	// taken from Envelope.Src.Station. So `d.Station` cannot appear here, and
	// looking for a bare `station` would match almost anything in this file.
	//
	// Matching the ARGUMENT PAIR pins the position, not just the presence: this
	// is the actor slot of the applied-delta INSERT specifically, immediately
	// after payload_code. A future edit that keeps the variable but moves it out
	// of the actor position still fails here, which a substring check on the
	// identifier alone would not.
	if !strings.Contains(src, "d.PayloadCode, station,") {
		t.Error("the applier no longer passes the envelope station into the audit INSERT's " +
			"actor position — ListCycleEvents reads the actor column for the station and " +
			"would now return one nameless key for the whole site")
	}
}
