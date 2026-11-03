package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/domain"
)

// TestSpliceStepShift_MapsOntoThePersistedPlan pins the mapping against ORDER 7,
// the live wedge, spelled exactly as the rig produced it.
//
// Pre-splice the allocator saw:
//
//	0 pickup LSD_023   ← recorded in order_bins
//	1 dropoff SLN_003
//	2 wait ALN_003     (an operator wait, already in the plan)
//	3 pickup ALN_003   ← recorded in order_bins
//	...
//
// The splice inserted a lane wait ahead of step 0 and another ahead of the
// re-entry, so those two recorded positions became 1 and 4. Both junction rows
// stayed at 0 and 3, where the persisted plan has WAITS.
func TestSpliceStepShift_MapsOntoThePersistedPlan(t *testing.T) {
	t.Parallel()
	spliced := []resolvedStep{
		{Action: protocol.ActionWait, Node: "LSD-M1-WAIT", WaitKind: WaitKindLane, WaitLane: 27}, // inserted
		{Action: protocol.ActionPickup, Node: "LSD_023"},                                         // was 0
		{Action: protocol.ActionDropoff, Node: "SLN_003"},                                        // was 1
		{Action: protocol.ActionWait, Node: "ALN_003"},                                           // was 2 — NOT a lane wait
		{Action: protocol.ActionPickup, Node: "ALN_003"},                                         // was 3
		{Action: protocol.ActionDropoff, Node: "SLN_004"},                                        // was 4
		{Action: protocol.ActionWait, Node: "LSD-M1-WAIT", WaitKind: WaitKindLane, WaitLane: 27}, // inserted
		{Action: protocol.ActionDropoff, Node: "LSD_022"},                                        // was 5
	}

	got := spliceStepShift(spliced)
	want := map[int]int{0: 1, 1: 2, 2: 3, 3: 4, 4: 5, 5: 7}
	if len(got) != len(want) {
		t.Fatalf("shift has %d entries, want %d: %v", len(got), len(want), got)
	}
	for pre, post := range want {
		if got[pre] != post {
			t.Errorf("pre-splice step %d maps to %d, want %d. The junction row filed under "+
				"%d must land on the step the persisted plan actually has there", pre, got[pre], post, pre)
		}
	}

	// The operator wait at pre-splice 2 is NOT treated as an insertion: only the
	// splice's own WaitKindLane steps are. Mistaking a plain wait for an inserted
	// one would shift everything after it by one too many.
	if got[3] != 4 {
		t.Errorf("an operator wait was counted as a splice insertion — pre-splice 3 landed at %d", got[3])
	}
}

// TestSpliceStepShift_UngatedPlanIsUntouched — the splice inserts nothing on an
// ungated path, so nothing may be rewritten. A shift on an unspliced plan would
// corrupt a correct junction, which is worse than the bug being fixed.
func TestSpliceStepShift_UngatedPlanIsUntouched(t *testing.T) {
	t.Parallel()
	plan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "A"},
		{Action: protocol.ActionDropoff, Node: "B"},
		{Action: protocol.ActionWait, Node: "C"},
		{Action: protocol.ActionPickup, Node: "C"},
	}
	if got := spliceStepShift(plan); len(got) != 0 {
		t.Errorf("ungated plan produced a shift %v — an unspliced plan's indices are already correct", got)
	}
}

// TestCartsPlanMismatch_CatchesTheDrift is the invariant that would have caught
// this the day it landed.
//
// The stale row is not malformed and points at a real step; the step at that
// position simply does not do what the row says (a wait where a pickup should
// be, a pickup where its set-down should be). That is the entire signal, and an
// index-only check has none of it.
func TestCartsPlanMismatch_CatchesTheDrift(t *testing.T) {
	t.Parallel()
	spliced := []resolvedStep{
		{Action: protocol.ActionWait, Node: "LSD-M1-WAIT", WaitKind: WaitKindLane, WaitLane: 27},
		{Action: protocol.ActionPickup, Node: "LSD_023"},
		{Action: protocol.ActionDropoff, Node: "SLN_003"},
	}
	lift := func(pick, drop int) []domain.Cart {
		return []domain.Cart{{BinID: 13, PickStep: &pick, DropStep: &drop}}
	}

	if err := cartsPlanMismatch(spliced, lift(0, 1)); err == nil {
		t.Fatal("a row whose pick step is a WAIT passed the check — this is exactly the state " +
			"orders 7 and 10 were in for 44 minutes, and it must not read as valid")
	}
	if err := cartsPlanMismatch(spliced, lift(1, 2)); err != nil {
		t.Errorf("the re-indexed row was rejected: %v", err)
	}
	if err := cartsPlanMismatch(spliced, lift(1, 9)); err == nil {
		t.Error("a row whose set-down points past the end of the plan passed the check")
	}
	if err := cartsPlanMismatch(spliced, lift(9, 10)); err == nil {
		t.Error("a row pointing past the end of the plan passed the check")
	}
	if err := cartsPlanMismatch(spliced[1:], lift(domain.InHand, 1)); err != nil {
		t.Errorf("a cart in hand from the start, set down at a dropoff, was rejected: %v", err)
	}
}
