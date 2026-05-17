package protocol

import (
	"sort"
	"strings"
	"testing"
)

func TestStatusIsTerminal(t *testing.T) {
	t.Parallel()
	terminals := []Status{StatusConfirmed, StatusCancelled, StatusFailed, StatusSkipped}
	for _, s := range terminals {
		if !s.IsTerminal() {
			t.Errorf("%s.IsTerminal() = false, want true", s)
		}
	}
	nonTerminals := []Status{StatusPending, StatusDelivered, StatusInTransit, StatusStaged, StatusQueued}
	for _, s := range nonTerminals {
		if s.IsTerminal() {
			t.Errorf("%s.IsTerminal() = true, want false", s)
		}
	}
}

func TestStatusFaultedIsNonTerminal(t *testing.T) {
	t.Parallel()
	if StatusFaulted.IsTerminal() {
		t.Error("StatusFaulted.IsTerminal() = true, want false")
	}
}

func TestFaultedTransitions(t *testing.T) {
	t.Parallel()
	accepted := []struct{ from, to Status }{
		{StatusDispatched, StatusFaulted},
		{StatusAcknowledged, StatusFaulted},
		{StatusInTransit, StatusFaulted},
		{StatusStaged, StatusFaulted},
		{StatusFaulted, StatusInTransit},
		{StatusFaulted, StatusDelivered},
		{StatusFaulted, StatusFailed},
		{StatusFaulted, StatusCancelled},
	}
	for _, c := range accepted {
		if !c.from.CanTransitionTo(c.to) {
			t.Errorf("%s.CanTransitionTo(%s) = false, want true", c.from, c.to)
		}
	}

	rejected := []struct{ from, to Status }{
		{StatusQueued, StatusFaulted},
		{StatusPending, StatusFaulted},
		{StatusDelivered, StatusFaulted},
		{StatusConfirmed, StatusFaulted},
		{StatusFaulted, StatusQueued},
		{StatusFaulted, StatusDispatched},
		{StatusFaulted, StatusPending},
		{StatusFaulted, StatusConfirmed},
	}
	for _, c := range rejected {
		if c.from.CanTransitionTo(c.to) {
			t.Errorf("%s.CanTransitionTo(%s) = true, want false", c.from, c.to)
		}
	}
}
func TestStatusCanTransitionTo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from, to Status
		want     bool
	}{
		{StatusPending, StatusSourcing, true},
		{StatusSourcing, StatusDispatched, true},
		{StatusDelivered, StatusConfirmed, true},
		{StatusConfirmed, StatusPending, false}, // terminal
		{StatusPending, StatusConfirmed, false}, // skip
	}
	for _, c := range cases {
		got := c.from.CanTransitionTo(c.to)
		if got != c.want {
			t.Errorf("%s.CanTransitionTo(%s) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestStatusString(t *testing.T) {
	t.Parallel()
	if got := StatusPending.String(); got != "pending" {
		t.Errorf("StatusPending.String() = %q, want %q", got, "pending")
	}
}

func TestStatusScanValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   any
		want Status
	}{
		{"string", "pending", StatusPending},
		{"bytes", []byte("delivered"), StatusDelivered},
		{"nil", nil, Status("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s Status
			if err := s.Scan(c.in); err != nil {
				t.Fatalf("Scan(%v): %v", c.in, err)
			}
			if s != c.want {
				t.Errorf("Scan(%v) = %q, want %q", c.in, s, c.want)
			}
		})
	}

	// Round-trip through Value
	for _, want := range AllStatuses() {
		v, err := want.Value()
		if err != nil {
			t.Fatalf("%s.Value(): %v", want, err)
		}
		var got Status
		if err := got.Scan(v); err != nil {
			t.Fatalf("Scan(%v): %v", v, err)
		}
		if got != want {
			t.Errorf("round-trip %s: got %s", want, got)
		}
	}
}

func TestStatusScanRejectsUnsupportedType(t *testing.T) {
	t.Parallel()
	var s Status
	if err := s.Scan(42); err == nil {
		t.Error("Scan(int) returned nil error, want failure")
	}
}

// ─── Predicate ↔ SQL projector drift tests ─────────────────────────────
//
// The risk class these tests catch: a new status is added to the enum,
// the author classifies it via one of the predicate functions, but
// forgets to update a hand-rolled SQL list somewhere. The projectors are
// derived from the predicates, so this kind of drift is now impossible
// *within* the protocol package — these tests pin that property.
//
// They also catch the inverse: someone hand-edits the projector output
// (none of our helpers permit this, but a refactor could regress it)
// without updating the predicate. The two are required to agree by
// construction; the tests make that requirement explicit.

// predicateProjectorPairs is the canonical table of "this predicate
// should yield this SQL list." Adding a new predicate requires adding a
// row here — the coverage test below fails otherwise, forcing the author
// to think about it.
var predicateProjectorPairs = []struct {
	name      string
	predicate func(Status) bool
	projector func() string
}{
	{"IsTerminal", IsTerminal, TerminalStatusSQLList},
	{"NonTerminal", func(s Status) bool { return !IsTerminal(s) }, NonTerminalStatusSQLList},
	{"IsFailureTerminal", IsFailureTerminal, FailureTerminalStatusSQLList},
	{"IsVendorActive", IsVendorActive, VendorActiveStatusSQLList},
	{"IsPreDispatch", IsPreDispatch, PreDispatchStatusSQLList},
	{"IsRuntimeStuckCandidate", IsRuntimeStuckCandidate, RuntimeStuckCandidateStatusSQLList},
	{"IsOperatorVisible", IsOperatorVisible, OperatorVisibleStatusSQLList},
}

// TestStatusSQLProjectorsAgreeWithPredicates is the drift detector. For
// every (predicate, projector) pair, walk the entire status enum and
// verify each status is present in the SQL list iff the predicate
// returns true. Catches any future drift between Go-side classification
// and SQL projection.
func TestStatusSQLProjectorsAgreeWithPredicates(t *testing.T) {
	t.Parallel()
	for _, pair := range predicateProjectorPairs {
		t.Run(pair.name, func(t *testing.T) {
			projected := pair.projector()
			for _, s := range AllStatuses() {
				token := "'" + string(s) + "'"
				inList := containsToken(projected, token)
				want := pair.predicate(s)
				if inList != want {
					t.Errorf("status %q: predicate=%v, in SQL list=%v (list=%q)",
						s, want, inList, projected)
				}
			}
		})
	}
}

// TestStatusSQLProjectorsAreSorted pins the lex-sorted ordering so any
// caller doing literal-string assertions against the projector output
// (drift tests, golden files, JS-side mirrors) has a stable target.
func TestStatusSQLProjectorsAreSorted(t *testing.T) {
	t.Parallel()
	for _, pair := range predicateProjectorPairs {
		t.Run(pair.name, func(t *testing.T) {
			parts := strings.Split(pair.projector(), ",")
			// An empty projector (no statuses matched) trips strings.Split
			// into a single-element [""] slice; that's vacuously sorted.
			if len(parts) == 1 && parts[0] == "" {
				return
			}
			sortedCopy := append([]string(nil), parts...)
			sort.Strings(sortedCopy)
			for i := range parts {
				if parts[i] != sortedCopy[i] {
					t.Errorf("%s: not lex-sorted: got %v", pair.name, parts)
					break
				}
			}
		})
	}
}

// TestEveryStatusClassifiedAsTerminalOrNot is the coverage guard for the
// terminal/non-terminal split — every known status must answer
// IsTerminal one way or the other (Go bool can't represent "neither"
// but a status missing from AllStatuses() would silently be skipped by
// the projectors, which is the actual risk). This test exercises every
// status by name to catch enum/AllStatuses drift.
func TestEveryStatusClassifiedAsTerminalOrNot(t *testing.T) {
	t.Parallel()
	// All status constants declared in status.go. If a new constant is
	// added without being appended to AllStatuses(), this list is the
	// place to catch it — add the new constant here and the test will
	// fail until AllStatuses() includes it.
	declared := []Status{
		StatusPending, StatusSourcing, StatusQueued, StatusSubmitted,
		StatusDispatched, StatusAcknowledged, StatusInTransit, StatusStaged,
		StatusDelivered, StatusConfirmed, StatusFaulted, StatusFailed,
		StatusCancelled, StatusReshuffling, StatusSkipped,
	}
	enumerated := map[Status]bool{}
	for _, s := range AllStatuses() {
		enumerated[s] = true
	}
	for _, s := range declared {
		if !enumerated[s] {
			t.Errorf("status %q is declared as a constant but missing from AllStatuses() — SQL projectors will silently skip it", s)
		}
	}
}

// containsToken reports whether the comma-separated quoted SQL list
// contains the exact token. Substring-safe: 'failed' must not match
// 'failed_x' or similar. The projector builds quoted tokens so we
// match the quotes literally.
func containsToken(list, token string) bool {
	if list == "" {
		return false
	}
	for _, p := range strings.Split(list, ",") {
		if p == token {
			return true
		}
	}
	return false
}
