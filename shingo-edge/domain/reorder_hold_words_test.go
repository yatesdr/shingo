package domain

import (
	"testing"

	"shingo/protocol"
)

// TestReorderHoldChip: a held tile's chip is the point its claim asks at and
// the cause in two or three words. An off says which of its two writers it
// was: the idle rule's (its note, IdleOffNote) or an older Core's give-up. A
// person's off draws no chip where the board's header carries the line's
// switch, and draws one where it does not. Before 2026-11-14 the board spelled
// these words itself from the kind ("AUTO-REORDER OFF" for both offs), pinned
// by operator-loader-station.test.js section 21.
func TestReorderHoldChip(t *testing.T) {
	t.Parallel()
	consume := &NodeClaim{Role: protocol.ClaimRoleConsume, ReorderPoint: 110, UOPCapacity: 240, AutoReorder: true}
	// A produce claim with no reorder point asks at its pack (DemandLevel).
	produce := &NodeClaim{Role: protocol.ClaimRoleProduce, UOPCapacity: 20, AutoReorder: true}
	const coreNote = "Core gave up waiting on order 12 (abandoned: waited 1h0m0s)"
	for _, c := range []struct {
		name    string
		claim   *NodeClaim
		hold    ReorderHold
		note    string
		onBoard bool
		want    string
	}{
		{"nothing held", consume, ReorderHoldNone, "", false, ""},
		{"paused", consume, ReorderHoldPaused, "a person cancelled order 12", false, "REORDER AT 110: PAUSED"},
		{"the idle rule's off", consume, ReorderHoldOff, IdleOffNote("1 hour"), false, "REORDER AT 110: LINE IDLE"},
		{"an older Core's off", consume, ReorderHoldOff, coreNote, false, "REORDER AT 110: CORE GAVE UP"},
		{"a person's off, the switch on the board", consume, ReorderHoldPersonOff, "LINE-OPS", true, ""},
		{"a person's off, no switch on the board", consume, ReorderHoldPersonOff, "LINE-OPS", false, "REORDER AT 110: SWITCHED OFF"},
		{"an idle off, the switch on the board", consume, ReorderHoldOff, IdleOffNote("1 hour"), true, "REORDER AT 110: LINE IDLE"},
		{"a count that cannot be the claim's", consume, ReorderHoldCount, "", false, "REORDER AT 110: CHECK COUNT"},
		{"a kind this Edge does not know", consume, ReorderHold("rested"), "", false, "REORDER AT 110: HELD"},
		{"produce, no reorder point: its pack", produce, ReorderHoldPaused, "order 3 failed", false, "REORDER AT 20: PAUSED"},
	} {
		if got := ReorderHoldChip(c.claim, c.hold, c.note, c.onBoard); got != c.want {
			t.Errorf("%s: chip %q, want %q", c.name, got, c.want)
		}
	}
}
