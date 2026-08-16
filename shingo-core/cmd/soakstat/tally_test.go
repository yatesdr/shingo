package main

import "testing"

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
// The rows below are the real log lines from the 2026-08-10 run.
func TestTallyValue_ReadsTheValueNotTheRepetition(t *testing.T) {
	t.Parallel()
	const bypassLine = `2026/08/10 12:24:38 burial-shadow BYPASS=1 (expected 0) — placements buried a ` +
		`hard-claimed bin without going through the store-slot selector; grep "burial-shadow: GUARD BYPASS"`
	const tallyLine = `2026/08/10 12:24:38 burial-shadow tally (since boot): soft-hold burials 7 ` +
		`(longest held at burial 5s), dig-uncovered 0`

	if n, ok := tallyValue(bypassLine, "BYPASS="); !ok || n != 1 {
		t.Errorf("BYPASS= read as (%d, %v), want (1, true)", n, ok)
	}
	if n, ok := tallyValue(tallyLine, "soft-hold burials "); !ok || n != 7 {
		t.Errorf("soft-hold burials read as (%d, %v), want (7, true)", n, ok)
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
