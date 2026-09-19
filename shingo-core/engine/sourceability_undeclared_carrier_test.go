package engine

import (
	"strings"
	"testing"

	"shingocore/store/sourceability"
)

// sourceability_undeclared_carrier_test.go — the operator sentence when the
// stock exists and cannot be fetched.
//
// ── WHY THE SENTENCE NEEDED A SECOND HALF ───────────────────────────────────
//
// A carrier holding a payload its bin type is not declared to carry counts as
// stock on every inventory surface and is refused by every sourcing reader. So
// the style goes RED — "missing PART-A" — beside an inventory page saying the
// parts are there. The operator's action for "there are no parts" is to have
// more made; for "the parts are in the wrong carrier" it is to fetch someone
// who can move them or declare the bin type. Those are different people and
// different hours, and a sentence that cannot tell them apart sends the wrong
// one.
//
// ── TWO FACTS, TWO SENTENCES, AND THE PAYLOAD STAYS MISSING ─────────────────
//
// The payload is not removed from the missing list: it genuinely is missing,
// nothing sourceable exists, and Missing is what the wire and the /sourcing
// page index blocked changeovers by. The second sentence says WHY, and it names
// the fix rather than a person.
//
// ── THE QUIET ARM IS THE ONE THAT MATTERS MOST ──────────────────────────────
//
// On a plant with no findings the RED sentence must be byte-for-byte what it
// was before any of this existed. Springfield has payload_bin_types rows for
// one payload of 128, so the quiet arm is the arm that runs.

func redState(missing []string, flagged ...sourceability.PayloadCount) sourceability.StyleState {
	return sourceability.StyleState{
		Status:             sourceability.StatusRed,
		Missing:            missing,
		UndeclaredCarriers: flagged,
	}
}

// TestReasonFor_RedWithNoFindingsIsUnchanged is the quiet arm, and it is
// written as a literal rather than as a comparison against a helper so that a
// change to the sentence has to be made HERE, deliberately, by someone who read
// this comment.
func TestReasonFor_RedWithNoFindingsIsUnchanged(t *testing.T) {
	got := reasonFor(redState([]string{"PART-A", "PART-B"}))
	const want = "Cannot change over — missing PART-A, PART-B."
	if got != want {
		t.Errorf("reasonFor = %q, want %q.\n"+
			"A plant with nothing flagged must read exactly as it did before the "+
			"carrier-rule finding existed — that is every plant on an ordinary day, "+
			"and a changed sentence there is a changed sentence for no reason.", got, want)
	}
}

// TestReasonFor_NamesTheFlaggedStockInASecondSentence.
func TestReasonFor_NamesTheFlaggedStockInASecondSentence(t *testing.T) {
	got := reasonFor(redState([]string{"PART-A"},
		sourceability.PayloadCount{PayloadCode: "PART-A", Bins: 2}))
	const want = "Cannot change over — missing PART-A. " +
		"2 bins of PART-A in undeclared carriers — not sourceable; resolve bin type."
	if got != want {
		t.Errorf("reasonFor = %q, want %q", got, want)
	}

	// TWO SENTENCES, NOT ONE. "No stock" and "stock in the wrong carrier" have
	// different actions; a reader who takes the first action for the second
	// problem orders parts that are already in the building.
	first, rest, found := strings.Cut(got, ". ")
	if !found || strings.Contains(first, "undeclared") || !strings.Contains(rest, "undeclared") {
		t.Errorf("the two facts share a sentence: %q", got)
	}
}

// TestReasonFor_SingularReadsAsOneBin. "1 bins" is the kind of thing an
// operator stops trusting a screen over.
func TestReasonFor_SingularReadsAsOneBin(t *testing.T) {
	got := reasonFor(redState([]string{"PART-A"},
		sourceability.PayloadCount{PayloadCode: "PART-A", Bins: 1}))
	if !strings.Contains(got, "1 bin of PART-A") {
		t.Errorf("reasonFor = %q, want it to say \"1 bin of PART-A\"", got)
	}
}

// TestReasonFor_OnlyTheFlaggedPayloadsAreNamed. A style missing two payloads
// where only one has flagged stock must not imply the other is a carrier
// problem: the first sentence still lists both, the second lists one.
func TestReasonFor_OnlyTheFlaggedPayloadsAreNamed(t *testing.T) {
	got := reasonFor(redState([]string{"PART-A", "PART-B"},
		sourceability.PayloadCount{PayloadCode: "PART-A", Bins: 3}))
	if !strings.Contains(got, "missing PART-A, PART-B.") {
		t.Errorf("the missing list lost a payload: %q", got)
	}
	_, second, _ := strings.Cut(got, "missing PART-A, PART-B.")
	if !strings.Contains(second, "PART-A") || strings.Contains(second, "PART-B") {
		t.Errorf("the finding sentence names the wrong payloads: %q", second)
	}
}

// TestReasonFor_TheOtherStatusesAreUntouched. The finding is scoped to RED
// because a satisfiable style's flagged carrier is a maintenance job, not a
// changeover blocker, and putting it in front of an operator mid-changeover is
// the wrong moment for it. /material-flags and /inventory are where it belongs.
func TestReasonFor_TheOtherStatusesAreUntouched(t *testing.T) {
	flagged := []sourceability.PayloadCount{{PayloadCode: "PART-A", Bins: 9}}
	for _, tc := range []struct {
		status sourceability.Status
		want   string
	}{
		{sourceability.StatusGreen, "Can change over."},
		{sourceability.StatusNotConfigured,
			"Not set up — no sourceability claims configured for this style."},
	} {
		s := sourceability.StyleState{Status: tc.status, UndeclaredCarriers: flagged}
		if got := reasonFor(s); got != tc.want {
			t.Errorf("reasonFor(%s) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// TestWireChanged_FollowsTheFlaggedPayloadListNotTheCount.
//
// Whether a missing payload has stock in an undeclared carrier changes the
// operator's ACTION, so it is an operator-visible change and belongs on the
// wire immediately. How MANY such bins there are is magnitude — the same class
// as time-to-empty drift, which this function deliberately ignores — and
// publishing on it would put a feed message on the wire every time one bin of
// thirty was moved.
func TestWireChanged_FollowsTheFlaggedPayloadListNotTheCount(t *testing.T) {
	none := redState([]string{"PART-A"})
	two := redState([]string{"PART-A"}, sourceability.PayloadCount{PayloadCode: "PART-A", Bins: 2})
	three := redState([]string{"PART-A"}, sourceability.PayloadCount{PayloadCode: "PART-A", Bins: 3})

	if !wireChanged(none, two) {
		t.Error("a missing payload gaining flagged stock did not register as a change — " +
			"the operator sentence changed and nothing was published")
	}
	if !wireChanged(two, none) {
		t.Error("a finding clearing did not register as a change — the sentence would " +
			"keep telling an operator to resolve a bin type that is already resolved")
	}
	if wireChanged(two, three) {
		t.Error("a bin count moving 2 -> 3 registered as a change. That is magnitude, " +
			"not a different verdict, and publishing on it is steady-state chatter.")
	}
}
