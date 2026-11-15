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

// TestAutoReorderHeld_Count is the arming check's boundaries: produce holds
// below 0 and past two packs, consume past one pack, nothing with no pack, and
// a hold on the row wins over the count.
func TestAutoReorderHeld_Count(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		role  protocol.ClaimRole
		pack  int
		count int
		row   ReorderHold
		want  ReorderHold
	}{
		{name: "produce at zero", role: protocol.ClaimRoleProduce, pack: 40, count: 0},
		{name: "produce below zero", role: protocol.ClaimRoleProduce, pack: 40, count: -1, want: ReorderHoldCount},
		{name: "produce at two packs", role: protocol.ClaimRoleProduce, pack: 40, count: 80},
		{name: "produce past two packs", role: protocol.ClaimRoleProduce, pack: 40, count: 81, want: ReorderHoldCount},
		{name: "consume below zero", role: protocol.ClaimRoleConsume, pack: 40, count: -1650},
		{name: "consume at a pack", role: protocol.ClaimRoleConsume, pack: 40, count: 40},
		{name: "consume past a pack", role: protocol.ClaimRoleConsume, pack: 40, count: 41, want: ReorderHoldCount},
		{name: "no pack known", role: protocol.ClaimRoleProduce, count: 44640},
		{name: "an off on the row wins", role: protocol.ClaimRoleProduce, pack: 40, count: 81, row: ReorderHoldOff, want: ReorderHoldOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &NodeClaim{Role: tc.role, SwapMode: protocol.SwapModeSingleRobot, AutoReorder: true, UOPCapacity: tc.pack}
			rt := &RuntimeState{RemainingUOPCached: tc.count, ReorderHold: tc.row, ReorderHoldAt: now}
			if got := AutoReorderHeld(c, rt, now); got.Hold != tc.want {
				t.Fatalf("held by %q (%q), want %q", got.Hold, got.Sentence, tc.want)
			}
		})
	}
}
