package main

import (
	"strings"
	"testing"

	"shingocore/service"
)

// TestTallyValue_ReadsTheValueNotTheRepetition pins the fix for a bug in the
// MEASUREMENT, which is the worst place to have one: it made a green run look
// catastrophic and would have made a catastrophic one look green.
//
// logBurialShadow re-emits its tally on every reconciliation sweep — every two
// seconds — for as long as the tally is non-zero. soakstat counted OCCURRENCES,
// so a single guard bypass was reported as `hard-burials 22`, then 322, growing
// linearly with uptime. On the summary line that number is labelled a tripwire
// whose expected value is zero, so the reading was not merely wrong, it was wrong
// in the direction that panics the reader.
//
// The rows below are the CURRENT emitter's lines. They were updated when the
// PLAN R.4 split turned one hard-burial counter into two — a parser fixture
// showing a format the emitter no longer produces is a test that passes while
// describing a system that does not exist.
func TestTallyValue_ReadsTheValueNotTheRepetition(t *testing.T) {
	t.Parallel()
	const bypassLine = `2026/08/12 12:24:38 burial-shadow BYPASS=1 (expected 0) — placements buried a ` +
		`hard claim that ALREADY EXISTED when the placing order was committed, so the store-slot ` +
		`selector was never asked. Find the placement path and route it through ` +
		`nodes.FindStoreSlotInLaneExcluding. THIS COUNT is the number, not a grep of it; for the ` +
		`per-event lines search the journal for "GUARD" followed by "BYPASS" — split here so this ` +
		`line stays out of its own results.`
	const churnLine = `2026/08/12 12:24:38 burial-shadow CHURN=4 — approved-then-invalidated: the ` +
		`buried claim arrived AFTER the placing order was committed and driving`
	const tallyLine = `2026/08/12 12:24:38 burial-shadow tally (since boot): soft-hold burials 7 ` +
		`(longest held at burial 5s), dig-uncovered 0`

	if n, ok := tallyValue(bypassLine, "BYPASS="); !ok || n != 1 {
		t.Errorf("BYPASS= read as (%d, %v), want (1, true)", n, ok)
	}
	if n, ok := tallyValue(churnLine, "CHURN="); !ok || n != 4 {
		t.Errorf("CHURN= read as (%d, %v), want (4, true) — the accepted population needs its own "+
			"reading, or the split hides it instead of separating it", n, ok)
	}
	if n, ok := tallyValue(tallyLine, "soft-hold burials "); !ok || n != 7 {
		t.Errorf("soft-hold burials read as (%d, %v), want (7, true)", n, ok)
	}

	// THE SELF-MATCHING GUARD, on the line that carries a search instruction. A
	// should-be-zero tally that contains its own grep pattern is counted by that
	// grep, so the reading is tally-lines-plus-events and the counter never reads
	// zero again (PLAN R.9, and it is why this line spells the marker in halves).
	if strings.Contains(bypassLine, service.BurialBypassMarker) {
		t.Errorf("the BYPASS tally line contains %q, the very string it tells the reader to search "+
			"for — grepping it counts this line too", service.BurialBypassMarker)
	}

	// A hundred repetitions of the same tally must read as its VALUE, not as 100.
	// This is the assertion the old code failed.
	last := 0
	for i := 0; i < 100; i++ {
		if n, ok := tallyValue(bypassLine, "BYPASS="); ok {
			last = n
		}
	}
	if last != 1 {
		t.Errorf("after 100 identical tally lines the reading is %d, want 1 — counting repetitions "+
			"measures uptime since the first burial, not burials", last)
	}
}

// TestTallyValue_DegradesToNoReading covers the direction a parser must fail in.
//
// A format change should produce "no reading" rather than a confident wrong one:
// the caller leaves the previous value alone on !ok, so a missing prefix or a
// non-numeric field cannot silently zero a tripwire.
func TestTallyValue_DegradesToNoReading(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, line, prefix string }{
		{"prefix absent", "burial-shadow tally: nothing here", "BYPASS="},
		{"prefix present, no digits", "burial-shadow BYPASS=none (expected 0)", "BYPASS="},
		{"prefix at end of line", "burial-shadow BYPASS=", "BYPASS="},
	}
	for _, c := range cases {
		if n, ok := tallyValue(c.line, c.prefix); ok {
			t.Errorf("%s: read (%d, true), want ok=false so the caller keeps its last good value", c.name, n)
		}
	}
}
