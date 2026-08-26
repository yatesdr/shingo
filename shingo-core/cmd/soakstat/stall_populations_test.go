package main

import (
	"strings"
	"testing"

	"shingo/protocol"
)

// TestStallPopulationsPartitionTheNonTerminalStatuses is the guard the stall
// checker did not have, and its absence was a live hole.
//
// The three kinds were written as SQL clauses by hand: `status='staged'`,
// `status='queued'`, and "not terminal and not (queued, staged, pending,
// sourcing)". Read together, `pending` and `sourcing` match NONE of them — an
// order sitting in either was watched by nothing, and those are precisely the
// pre-dispatch statuses where a held compound leg and a fleet-refused leg wait.
// Each clause was defensible alone. Nothing connected them.
//
// A PARTITION is the property that connects them: every non-terminal status in
// exactly one kind. It is what makes "nothing is stalled" a statement about the
// whole plant instead of about the statuses somebody remembered, and it is what
// a new status cannot silently fall out of — adding one to protocol fails this
// test until a kind claims it.
func TestStallPopulationsPartitionTheNonTerminalStatuses(t *testing.T) {
	t.Parallel()
	for _, s := range protocol.AllStatuses() {
		if protocol.IsTerminal(s) {
			continue
		}
		var claimed []string
		for _, p := range stallPopulations {
			if p.match(s) {
				claimed = append(claimed, p.label)
			}
		}
		switch len(claimed) {
		case 1:
			// exactly one kind owns it
		case 0:
			t.Errorf("status %q is non-terminal and belongs to NO stall population — an order "+
				"sitting in it forever is invisible to the checker. This is how `pending` and "+
				"`sourcing` went unwatched: every clause was written separately and none of them "+
				"covered the gap between them", s)
		default:
			t.Errorf("status %q belongs to %d stall populations (%s) — it would be reported twice, "+
				"under two different thresholds, and the shorter one would always win",
				s, len(claimed), strings.Join(claimed, ", "))
		}
	}
}

// TestStallPopulationsExcludeTerminalStatuses is the other half. A terminal order
// has stopped making progress BY DEFINITION; flagging one as stalled would bury
// every real finding under the plant's whole history.
func TestStallPopulationsExcludeTerminalStatuses(t *testing.T) {
	t.Parallel()
	for _, s := range protocol.AllStatuses() {
		if !protocol.IsTerminal(s) {
			continue
		}
		for _, p := range stallPopulations {
			if p.match(s) {
				t.Errorf("terminal status %q is claimed by the %q stall population — every "+
					"confirmed order in the run would be reported as stalled", s, p.label)
			}
		}
	}
}

// TestStallPopulationClausesAreRealSQL pins the rendering, not just the
// predicates: a population is only as good as the query it becomes.
//
// The empty case is the one worth asserting. `status IN ()` is a Postgres syntax
// error, so a kind whose predicate matched nothing would not report zero rows —
// it would fail the query and be skipped silently by the caller's `continue`,
// which reads exactly like "nothing is stalled".
func TestStallPopulationClausesAreRealSQL(t *testing.T) {
	t.Parallel()
	for _, p := range stallPopulations {
		c := p.clause()
		if strings.Contains(c, "IN ()") {
			t.Errorf("%q renders %q — an empty IN list is a syntax error, and the caller skips a "+
				"failed query without saying so", p.label, c)
		}
		if c == "FALSE" {
			t.Errorf("%q matches no status at all, so the kind is dead and its threshold is "+
				"never applied to anything", p.label)
		}
	}
}
