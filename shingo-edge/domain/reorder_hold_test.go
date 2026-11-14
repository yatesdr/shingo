package domain

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol"
)

// TestAutoReorderHeld is the decider's table: what holds an armed position's
// automatic ask, and the board's sentence for it. A pause holds for exactly
// ReorderPause and not a second past it; the other kinds hold until a write
// moves them.
func TestAutoReorderHeld(t *testing.T) {
	t.Parallel()
	set := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	armed := &NodeClaim{Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot, AutoReorder: true}
	unarmed := &NodeClaim{Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot}
	loader := &NodeClaim{Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeManualSwap, AutoReorder: true}
	row := func(h ReorderHold) *RuntimeState {
		return &RuntimeState{ReorderHold: h, ReorderHoldAt: set, ReorderHoldNote: "order 7 was cancelled"}
	}
	for _, tc := range []struct {
		name  string
		claim *NodeClaim
		rt    *RuntimeState
		at    time.Duration // after the hold was set
		want  ReorderHold   // "" = nothing holds
		says  string        // in the sentence
	}{
		{name: "no hold", claim: armed, rt: row(ReorderHoldNone)},
		{name: "a pause at its start", claim: armed, rt: row(ReorderHoldPaused), want: ReorderHoldPaused, says: "paused until"},
		{name: "a pause a second before its end", claim: armed, rt: row(ReorderHoldPaused), at: ReorderPause - time.Second,
			want: ReorderHoldPaused, says: "order 7 was cancelled"},
		{name: "a pause at its end", claim: armed, rt: row(ReorderHoldPaused), at: ReorderPause},
		{name: "off, a day later", claim: armed, rt: row(ReorderHoldOff), at: 24 * time.Hour, want: ReorderHoldOff, says: "is off"},
		{name: "a person's off, a week later", claim: armed, rt: row(ReorderHoldPersonOff), at: 7 * 24 * time.Hour,
			want: ReorderHoldPersonOff, says: "switched off here by order 7 was cancelled"},
		{name: "a kind this Edge does not know", claim: armed, rt: row("snoozed"), want: "snoozed", says: "does not know"},
		{name: "an unarmed claim", claim: unarmed, rt: row(ReorderHoldOff)},
		{name: "a loader's claim", claim: loader, rt: row(ReorderHoldOff)},
		{name: "no claim", rt: row(ReorderHoldOff)},
		{name: "no runtime row", claim: armed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := AutoReorderHeld(tc.claim, tc.rt, set.Add(tc.at))
			if got.Hold != tc.want || got.Holds() != (tc.want != "") {
				t.Fatalf("held = %+v, want kind %q (holds %v)", got, tc.want, tc.want != "")
			}
			if !strings.Contains(got.Sentence, tc.says) {
				t.Errorf("sentence %q does not say %q", got.Sentence, tc.says)
			}
			if tc.want == ReorderHoldPaused && !got.Until.Equal(set.Add(ReorderPause)) {
				t.Errorf("a pause set at %v ends at %v, want %v", set, got.Until, set.Add(ReorderPause))
			}
		})
	}
}

// TestReorderHold_Rank pins the order the write guards on: every holding kind
// ranks above none, and above the one before it in ReorderHolds.
func TestReorderHold_Rank(t *testing.T) {
	t.Parallel()
	if ReorderHoldNone.Rank() != 0 || ReorderHold("snoozed").Rank() != 0 {
		t.Fatal("none and an unknown word must rank 0")
	}
	want := []ReorderHold{ReorderHoldPaused, ReorderHoldOff, ReorderHoldPersonOff}
	for i, k := range want {
		if k.Rank() != i+1 {
			t.Errorf("%q ranks %d, want %d (paused < off < a person's off)", k, k.Rank(), i+1)
		}
	}
}
