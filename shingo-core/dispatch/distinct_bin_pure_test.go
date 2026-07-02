// distinct_bin_pure_test.go — the relay distinct-bin discriminator (commit 2).
// Pure computation over the resolved step list; runs without Docker.

package dispatch

import (
	"testing"

	"shingo/protocol"
)

// TestDistinctBinNeeds_SwapRelay pins the discriminator against the single-robot
// swap relay shape (shingo-edge BuildSingleSwapSteps, material_orders.go): 4
// pickup ACTIONS collapse to 2 distinct source NEEDS; the 2 staging re-grabs are
// silently excluded. So a reserve over this list finds exactly 2 bins with zero
// phantom misses even though the staging nodes are empty at dispatch.
func TestDistinctBinNeeds_SwapRelay(t *testing.T) {
	// Mirrors BuildSingleSwapSteps' 9-step shape.
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "src"},         // 1 true source (new bin)
		{Action: protocol.ActionDropoff, Node: "inStaging"},  // 2 park new
		{Action: "wait", Node: "coreNode"},                   // 3 drive + hold
		{Action: protocol.ActionPickup, Node: "coreNode"},    // 4 true source (old bin)
		{Action: protocol.ActionDropoff, Node: "outStaging"}, // 5 park old
		{Action: protocol.ActionPickup, Node: "inStaging"},   // 6 re-grab new
		{Action: protocol.ActionDropoff, Node: "coreNode"},   // 7 deliver new
		{Action: protocol.ActionPickup, Node: "outStaging"},  // 8 re-grab old
		{Action: protocol.ActionDropoff, Node: "outDest"},    // 9 deliver old
	}

	needs := distinctSourceNeeds(steps)

	gotNodes := make([]string, len(needs))
	for i, s := range needs {
		gotNodes[i] = s.Node
		if s.Action != protocol.ActionPickup {
			t.Errorf("need %d action = %q, want pickup", i, s.Action)
		}
	}
	want := []string{"src", "coreNode"}
	if len(gotNodes) != len(want) {
		t.Fatalf("distinctSourceNeeds = %v (%d needs), want %v (%d) — 4 pickups must collapse to 2 distinct sources",
			gotNodes, len(gotNodes), want, len(want))
	}
	for i := range want {
		if gotNodes[i] != want[i] {
			t.Errorf("need[%d] node = %q, want %q", i, gotNodes[i], want[i])
		}
	}
}

// TestDistinctBinNeeds_DropoffBeforePickupIsRegrab documents the discriminator's
// load-bearing edge: a pickup at N is a re-grab whenever an EARLIER step drops at
// N. So a builder that dropped a bin at N and then intended to pick a DIFFERENT
// (new) bin from N would be misclassified as a re-grab and silently skipped. The
// real swap builders never do this (a source pickup always precedes any dropoff
// to its node — occupancy also forbids two bins at one concrete node). This test
// trips if a future builder introduces the shape, forcing a revisit.
func TestDistinctBinNeeds_DropoffBeforePickupIsRegrab(t *testing.T) {
	steps := []resolvedStep{
		{Action: protocol.ActionDropoff, Node: "N"},
		{Action: protocol.ActionPickup, Node: "N"}, // classified as re-grab, NOT a source
	}
	if needs := distinctSourceNeeds(steps); len(needs) != 0 {
		t.Errorf("distinctSourceNeeds = %d need(s), want 0 (a pickup after a same-node dropoff is a re-grab)", len(needs))
	}
}

// TestDistinctBinNeeds_EmptyLegIsATrueSource confirms an empty-carrier pickup leg
// (produce "bring an empty to fill", Empty=true) at a real source with no prior
// dropoff is still a distinct need — the discriminator keys on relay position,
// not payload.
func TestDistinctBinNeeds_EmptyLegIsATrueSource(t *testing.T) {
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "emptyPool", Empty: true},
		{Action: protocol.ActionDropoff, Node: "produceLine"},
	}
	needs := distinctSourceNeeds(steps)
	if len(needs) != 1 || needs[0].Node != "emptyPool" || !needs[0].Empty {
		t.Errorf("distinctSourceNeeds = %+v, want a single empty-leg need at emptyPool", needs)
	}
}
