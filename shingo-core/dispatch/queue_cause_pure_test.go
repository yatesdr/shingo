package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// queue_cause_pure_test.go — the two things that can go wrong when literals
// become constants, neither of which the compiler catches.

// TestQueueCause_ValuesAreUnchanged pins every lane/gate cause to the exact
// string it had as a literal.
//
// THIS IS THE WHOLE RISK OF THE RENAME. Introducing the type cannot change
// behaviour; mistyping a value while introducing it can, silently and
// permanently. queue_cause is what an engineer groups by when a queue code
// trends wrong, so a value that shifts by one character splits one population
// into two in every query written against it, and the rows already on disk keep
// the old spelling forever. Nothing fails, nothing logs, and the histogram is
// just quietly wrong.
//
// Written as literals on the right-hand side ON PURPOSE. Comparing a constant
// to itself, or deriving the wanted value from the constant, would be the
// vacuous form §16 warns about — it would pass against any value at all. These
// strings were copied from the call sites this commit replaced, and that is what
// makes the test evidence rather than decoration.
//
// MUTATION (verified): change CauseLaneDigActive to "lane-dig-actice". This
// fires naming both spellings; nothing else in the suite does, because every
// producer and consumer moved to the constant together and agrees with itself.
func TestQueueCause_ValuesAreUnchanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		got  QueueCause
		want string
	}{
		{CauseLaneDeeperPending, "lane-deeper-pending"},
		{CauseLaneGroupActive, "lane-group-active"},
		{CauseLaneDigActive, "lane-dig-active"},
		{CauseLaneTargetBuried, "lane-target-buried"},
		{CauseLaneHeldDig, "lane-held-dig"},
		{CauseLaneHeldTraffic, "lane-held-traffic"},
		{CauseLaneHeldUnreadable, "lane-held-unreadable"},
		{CauseLaneOccupied, "lane-occupied"},
		{CauseLaneLocked, "lane-locked"},
		{CauseIntakeBuried, "intake-buried"},
		{CauseGateRebindUnavailable, "gate-rebind-unavailable"},
		{CauseGateAppendFailed, "gate-append-failed"},
		{CauseLaneAcquireError, "lane-acquire-error"},
		// These two are read BY VALUE outside this module: store/reconciliation
		// classifies a dwelling station wait by the cause string on the row, and
		// it cannot import this package (dispatch imports store, so the reverse
		// is a cycle). Its copies are causeStationWaitLiteral and
		// causeSwapPartnerFinishedLiteral, pinned to the same strings by
		// TestStationDwellCauseLiteralsMatchDispatch. A rename that touches only
		// one side does not fail to compile — it silently stops classifying, and
		// the anomaly board goes back to telling an operator to investigate a
		// robot fault that does not exist.
		{CauseStationWait, "station-wait"},
		{CauseSwapPartnerFinished, "swap-partner-finished"},
	} {
		if string(tc.got) != tc.want {
			t.Errorf("queue cause = %q, want %q — this value is already written on rows in the "+
				"plant's orders table and is grouped by in forensic queries; changing it splits one "+
				"population into two and nothing anywhere reports that it happened", tc.got, tc.want)
		}
	}
}

// TestQueueCause_NoFamilyLiteralsRemain is the decay guard.
//
// The type makes a TYPO a compile error. It does not make a LITERAL one:
// QueueCause("lane-dig-active") compiles and is exactly how this converges back
// to where it started, one convenient call site at a time. The conversion form
// has to stay legal — the ~15 causes outside this family still use it, and that
// visibility is deliberate — so the boundary is enforced here instead.
//
// Scope is the family's own values only. A non-family literal is not a
// violation; it is the follow-up work, marked in place.
//
// MUTATION (verified): put `QueueCause("lane-held-dig")` back at
// causeForLaneHolds. This fires naming the file and the value.
func TestQueueCause_NoFamilyLiteralsRemain(t *testing.T) {
	t.Parallel()
	family := []string{
		"lane-deeper-pending", "lane-group-active", "lane-dig-active", "lane-target-buried",
		"lane-held-dig", "lane-held-traffic", "lane-held-unreadable", "lane-occupied", "lane-locked", "intake-buried",
		"gate-rebind-unavailable", "gate-append-failed", "lane-acquire-error", "lane-entry-error",
		// "lock-race" is NOT here: fulfillment sets that same literal for an
		// unrelated bin race, so its presence as a literal is correct there. See
		// the type doc.
	}

	// Both modules — the family spans dispatch/ and fulfillment/, which is why
	// the constants are exported.
	for _, dir := range []string{".", "../fulfillment"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") ||
				strings.HasSuffix(name, "_test.go") || name == "queue_cause.go" {
				continue
			}
			path := filepath.Join(dir, name)
			b, rErr := os.ReadFile(path)
			if rErr != nil {
				t.Fatalf("read %s: %v", path, rErr)
			}
			src := string(b)
			for _, v := range family {
				if strings.Contains(src, `"`+v+`"`) {
					t.Errorf("%s still contains the literal %q — use the constant. A literal here "+
						"compiles, so nothing else catches it, and it is how the set stops being "+
						"enumerable", path, v)
				}
			}
		}
	}
}
