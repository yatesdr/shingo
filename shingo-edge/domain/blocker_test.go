package domain

import (
	"strings"
	"testing"
)

// TestBlockersToReasons_PreservesSentencesExactly is the byte-identity pin.
//
// canCompleteChangeover used to return []string and completeCutover joined it
// into "cannot cutover: <a>; <b>". Making blockers structured must not change
// one character of that message — the floor reads it, and the changeover
// post-mortems quote it. Blocker.Reason therefore carries the WHOLE sentence
// and this projection is a plain field read, never a re-render.
func TestBlockersToReasons_PreservesSentencesExactly(t *testing.T) {
	t.Parallel()

	blockers := []Blocker{
		{Reason: "task at node ALN_002 in staging_requested", NodeName: "ALN_002", Hard: true},
		{Reason: "order 703 in in_transit", OrderID: 703, Hard: true},
	}

	got := BlockersToReasons(blockers)
	want := []string{
		"task at node ALN_002 in staging_requested",
		"order 703 in in_transit",
	}
	if len(got) != len(want) {
		t.Fatalf("BlockersToReasons length = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("reason[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// The exact string completeCutover produces.
	if joined := "cannot cutover: " + strings.Join(got, "; "); joined !=
		"cannot cutover: task at node ALN_002 in staging_requested; order 703 in in_transit" {
		t.Errorf("joined toast = %q — the 400 message is a contract and must not drift", joined)
	}
}

// TestBlockersToReasons_NilAndEmpty pins that an unblocked gate produces a nil
// slice, not []string{}. canCompleteChangeover returns nil blockers on the
// success path and callers check len(); an empty-but-non-nil slice would still
// be falsy for len() but changes JSON encoding from null to [], so keep it
// deliberate rather than accidental.
func TestBlockersToReasons_NilAndEmpty(t *testing.T) {
	t.Parallel()

	if got := BlockersToReasons(nil); got != nil {
		t.Errorf("BlockersToReasons(nil) = %v, want nil", got)
	}
	if got := BlockersToReasons([]Blocker{}); got != nil {
		t.Errorf("BlockersToReasons(empty) = %v, want nil", got)
	}
}

// TestBlocker_HardIsDisplayOnly documents by assertion what the type's comment
// asserts in prose: nothing in this package branches on Hard. If a future
// change makes Hard control behaviour, that is the override the cutover gate
// is not allowed to have, and this test is the place it should be argued.
func TestBlocker_HardIsDisplayOnly(t *testing.T) {
	t.Parallel()

	hard := []Blocker{{Reason: "r", Hard: true}}
	soft := []Blocker{{Reason: "r", Hard: false}}

	if h, s := BlockersToReasons(hard), BlockersToReasons(soft); h[0] != s[0] {
		t.Errorf("Hard changed the projected sentence (%q vs %q) — it is display-only", h[0], s[0])
	}
}
