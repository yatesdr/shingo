package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBurialExclusionOneSpelling is standing law 3's drift test for "is this slot
// in front of a bin somebody is coming for".
//
// TWO CALLERS ASK IT. The store selector refuses to hand out a slot in front of a
// hard-claimed bin (nodes/lanes.go findStoreSlot); the dig's shuffle-slot search
// refuses to park a blocker there (SlotsBlockedByHardClaims, read by
// dispatch/reshuffle.go findShuffleSlots). They are different callers with
// different owner rules — the store exempts the requesting order, the dig
// deliberately does not — but "IN FRONT OF" must mean one thing, or the two
// guards protect different sets and the gap between them is exactly where a bin
// gets buried.
//
// The dig side reached this spelling the hard way. It used to read the expose
// bridge's pending_lane_extensions table, which protected only bins a FINISHED
// expose dig had uncovered — a narrower, differently-shaped set that happened to
// cover F-19 and nothing else. The bridge is gone; the question is asked of
// claims on both sides now, through this one helper.
//
// A SOURCE TEST because the two are different SQL statements over different
// shapes (one candidate versus a whole group), so no behavioural fixture can
// prove they share a definition — only that they agree on the cases somebody
// thought to write. What must not drift is the EXPRESSION, and that is checkable
// directly.
func TestBurialExclusionOneSpelling(t *testing.T) {
	sites := map[string]string{
		"the store selector's burial clause": filepath.Join("nodes", "lanes.go"),
		"the dig's shuffle-slot exclusion":   "lane_queries.go",
	}
	for what, path := range sites {
		src, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(src), "ShallowerInSameLane(") {
			t.Errorf("%s (%s) no longer goes through ShallowerInSameLane.\n"+
				"Both the store selector and the dig must spell \"in front of\" the same way — if they "+
				"drift, one guard protects a set the other does not and a bin gets buried in the gap. "+
				"If the geometry genuinely needs to change, change the helper.", what, path)
		}
		// Deliberately NOT also asserting "and does not mention the deleted bridge
		// table": both files carry past-tense tombstones explaining what the exclusion
		// used to read, which is exactly the record a future reader needs, and a test
		// that cannot tell a tombstone from a dependency would force those to be
		// deleted. That the bridge has no live readers is proved once, at the batch
		// level, by grep — not re-litigated per file here.
	}
}
