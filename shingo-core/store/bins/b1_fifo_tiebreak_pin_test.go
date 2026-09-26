package bins_test

import (
	"strings"
	"testing"
)

// fifoOrder returns the normalised ORDER BY of a finder's query body.
func fifoOrder(t *testing.T, file, fn string) string {
	t.Helper()
	body := readBody(t, file, fn)
	_, order, found := strings.Cut(body, "ORDER BY")
	if !found {
		t.Fatalf("%s has no ORDER BY; re-derive this test", fn)
	}
	order, _, _ = strings.Cut(order, "LIMIT")
	if i := strings.Index(order, "--"); i >= 0 {
		order = order[:i]
	}
	return strings.Join(strings.Fields(order), " ")
}

// TestB1_FIFOTieBreaksByID: the plant-wide FIFO finder orders by load time and
// then by bin id. Without the id, two bins loaded in the same instant (the
// seeder stamps loaded_at to the second; a burst of arrivals does the same) were
// chosen in whatever order Postgres returned them, and LIMIT 1 over that tie was
// not a decision (the planner golden flaked on exactly this, SHIM).
//
// WHY THE QUERY AND NOT A RUN. A behavioural test was tried first and measured:
// two sourceable SHIM bins at one loaded_at, asked, the chosen row rewritten to
// move its heap position, asked again. The lower id won five runs out of five
// with or without the tie-break, so a run cannot tell the two apart. What is
// deterministic is the ORDER BY itself.
//
// B1 flat bug (owner, 2026-09-25); was TestB1Pin_FIFOHasNoTieBreak, a verify-red.
func TestB1_FIFOTieBreaksByID(t *testing.T) {
	if got, want := fifoOrder(t, "bin_manifest.go", "func FindSourceFIFO("),
		"COALESCE(b.loaded_at, b.created_at) ASC, b.id ASC"; got != want {
		t.Fatalf("FindSourceFIFO orders by %q, want %q: a load-time tie must be broken by a unique key", got, want)
	}
}

// TestB1_BuriedFIFOTieBreaksByID is the same rule for the dig reader the picker
// reaches through the group resolver (GroupResolver.checkOldestBuried →
// store.FindOldestBuriedBin): which buried bin a strict-FIFO retrieve digs for.
// B1 flat bug (owner, 2026-09-26); was a verify-red of the same name shape.
func TestB1_BuriedFIFOTieBreaksByID(t *testing.T) {
	if got, want := fifoOrder(t, "../lane_queries.go", "func (db *DB) FindOldestBuriedBin("),
		"COALESCE(b.loaded_at, b.created_at) ASC, b.id ASC"; got != want {
		t.Fatalf("FindOldestBuriedBin orders by %q, want %q: a load-time tie must be broken by a unique key", got, want)
	}
}
