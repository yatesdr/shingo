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
	{"IsVendorTracked", IsVendorTracked, VendorTrackedStatusSQLList},
	{"IsPreDispatch", IsPreDispatch, PreDispatchStatusSQLList},
	{"IsAcquiring", IsAcquiring, AcquiringStatusSQLList},
	{"IsRuntimeStuckCandidate", IsRuntimeStuckCandidate, RuntimeStuckCandidateStatusSQLList},
	{"IsStuckSweepCandidate", IsStuckSweepCandidate, StuckSweepStatusSQLList},
	{"IsOperatorVisible", IsOperatorVisible, OperatorVisibleStatusSQLList},
}

// TestBlockingStatusesAreTracked is the invariant that was missing, and its
// absence was a live hole.
//
// A status that blocks a changeover is asserting that something physical is
// outstanding at a node and the order must reach a terminal state before work
// there can resume. The only thing that moves such an order along is the fleet
// poller, and the poller only watches orders it was told to track — which, after
// a restart, is whatever the boot query selects.
//
// So if a status can block a changeover and is not tracked, it is a block that
// nothing can clear. That was exactly true of `faulted`: the boot query selected
// IsVendorActive, faulted is not in it, and a faulted order surviving a restart
// was never polled again. Its grace period never expired, no sweep covered it
// (deliberately, on both counts), no anomaly was raised (also deliberately), and
// it went on blocking changeovers at its node until a person found it.
//
// Every one of those exclusions was defensible alone. Nothing connected them.
func TestBlockingStatusesAreTracked(t *testing.T) {
	t.Parallel()
	for _, s := range AllStatuses() {
		if !BlocksChangeoverStart(s) {
			continue
		}
		if !IsVendorTracked(s) {
			t.Errorf("status %q blocks a changeover but is not vendor-tracked.\n"+
				"Nothing polls it, so nothing can move it to a terminal state, so the block it creates never clears.\n"+
				"Either track it, or it must not block.", s)
		}
	}
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

// ─── Changeover-start classification ─────────────────────────────────────

// TestChangeoverStartActionIsExhaustive is the build-fails-on-a-new-status
// guard. Every status must be classified cancel / block / pass; the zero value
// is never a legitimate answer.
//
// It walks AllStatuses() rather than declaring its own list on purpose — a
// second hand-maintained status list is exactly the rot the derived helpers in
// this file exist to prevent, and AllStatuses() is already pinned against the
// declared constants by TestEveryStatusClassifiedAsTerminalOrNot.
func TestChangeoverStartActionIsExhaustive(t *testing.T) {
	t.Parallel()
	for _, s := range AllStatuses() {
		if got := ChangeoverStartActionFor(s); got == ChangeoverStartUnclassified {
			t.Errorf("status %q has no changeover-start classification — add it to "+
				"ChangeoverStartActionFor (cancel / block / pass) rather than letting it "+
				"fall through to the most permissive answer by accident", s)
		}
	}
	// The guard is only worth having if the default arm is reachable. A status
	// the switch does not name must come back unclassified — otherwise the loop
	// above passes vacuously and a new status would slip through.
	if got := ChangeoverStartActionFor(Status("not-a-real-status")); got != ChangeoverStartUnclassified {
		t.Fatalf("unnamed status classified %s — the default arm is unreachable and "+
			"the exhaustiveness guard above is decorative", got)
	}
}

// TestCancelledStatusesCannotBeHoldingABin is the SAFETY property, and it is
// the one that actually matters. Everything else about this classification is a
// judgement call; this is the invariant Hopkinsville paid for on 28 July, where
// cancelling a leg that was carrying the changeover's own empty carriers would
// have deadlocked it permanently.
//
// It is asserted against IsVendorActive, which is derived independently of the
// classifier, so this is a real cross-check rather than a restatement.
//
// THIS REPLACES A TEST THAT PINNED THE CANCEL SET TO IsPreDispatch. That test
// passed for months and enforced the SNF2 defect: IsPreDispatch belongs to the
// fulfillment scanner and excludes submitted/acknowledged, so pinning to it made
// "the classifier agrees with the scanner's predicate" the property under test,
// which nobody needed, while the property anybody cared about went unasserted. A
// test can hold two things in agreement and still be holding them both wrong.
func TestCancelledStatusesCannotBeHoldingABin(t *testing.T) {
	t.Parallel()
	for _, s := range AllStatuses() {
		if ChangeoverStartActionFor(s) != ChangeoverStartCancel {
			continue
		}
		if IsVendorActive(s) || s == StatusFaulted {
			t.Errorf("status %q is classified cancel but a robot may be holding a bin "+
				"for it — cancelling it can strand the changeover that needs the bin "+
				"(HK 2026-07-28)", s)
		}
	}
}

// TestBlockedStatusesAreExactlyTheVendorActiveOnes pins the block arm against
// the independent predicate. BlocksChangeoverStart now delegates to the
// classifier, so asserting the two agree would be circular; IsVendorActive does
// not, which is what makes this worth running.
func TestBlockedStatusesAreExactlyTheVendorActiveOnes(t *testing.T) {
	t.Parallel()
	for _, s := range AllStatuses() {
		got := ChangeoverStartActionFor(s) == ChangeoverStartBlock
		want := IsVendorActive(s) || s == StatusFaulted
		if got != want {
			t.Errorf("status %q: classified block=%v but vendor-active-or-faulted=%v", s, got, want)
		}
	}
}

// TestBlocksChangeoverStartMembership pins the set by name. The membership is
// the decision under review in four rounds, so it is asserted literally rather
// than derived — if someone edits the predicate, this test is the place the
// argument is re-read before the edit lands.
func TestBlocksChangeoverStartMembership(t *testing.T) {
	t.Parallel()
	want := map[Status]bool{
		StatusDispatched: true, StatusInTransit: true, StatusStaged: true,
		StatusFaulted: true,
	}
	for _, s := range AllStatuses() {
		if got := BlocksChangeoverStart(s); got != want[s] {
			t.Errorf("BlocksChangeoverStart(%q) = %v, want %v", s, got, want[s])
		}
		if got := s.BlocksChangeoverStart(); got != want[s] {
			t.Errorf("method form %q = %v, want %v", s, got, want[s])
		}
	}
}

// TestChangeoverStartNeverCancelsOrBlocksATerminalOrder guards the property
// that makes the classification safe to run over a live order set: a terminal
// order is already done, so it can be neither cancelled nor waited on.
func TestChangeoverStartNeverCancelsOrBlocksATerminalOrder(t *testing.T) {
	t.Parallel()
	for _, s := range AllStatuses() {
		if !IsTerminal(s) {
			continue
		}
		if a := ChangeoverStartActionFor(s); a != ChangeoverStartPass {
			t.Errorf("terminal status %q classified %s, want pass", s, a)
		}
	}
}

// TestAcknowledgedAndSubmittedAreCancelledNotBlockedAndNotIgnored carries BOTH
// halves of the decision for these two statuses, because carrying only the first
// half is precisely what went wrong.
//
// They must NOT BLOCK: nothing in either service reaps them (AbandonStuckOrders
// is scoped to {dispatched, staged}) and this HMI exposes no operator order
// cancel, so blocking would lock an operator out of changeover until Edge
// restarted.
//
// They must BE CANCELLED: Springfield SNF2, 30 July. Two complex orders for the
// outgoing style reached acknowledged thirteen seconds before an operator
// started a changeover. They correctly did not block — no robot had them — and
// they were then left alive, so the changeover raised its own orders for the
// incoming style against the same node and the line had two styles' deliveries
// in flight at once.
//
// The original test asserted the first half and "want pass" for the second,
// which reads as a decision and was an omission: the reasoning that established
// "must not block" was allowed to imply "therefore leave alone". A status that
// no robot is holding is exactly a status that should be cancelled.
func TestAcknowledgedAndSubmittedAreCancelledNotBlockedAndNotIgnored(t *testing.T) {
	t.Parallel()
	for _, s := range []Status{StatusAcknowledged, StatusSubmitted} {
		if BlocksChangeoverStart(s) {
			t.Errorf("%q must not block changeover start: nothing reaps it and the "+
				"operator has no way to clear it", s)
		}
		if a := ChangeoverStartActionFor(s); a != ChangeoverStartCancel {
			t.Errorf("%q classified %s, want cancel — leaving it alive lets an order for "+
				"the outgoing style outlive the changeover (SPR SNF2 2026-07-30)", s, a)
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
