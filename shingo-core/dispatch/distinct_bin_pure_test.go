// distinct_bin_pure_test.go — the relay distinct-bin discriminator.
// complexPickups is the SINGLE relay-discriminator in the tree: it flags each
// pickup's POTENTIAL relay status (an earlier same-order dropoff at the node)
// purely from the resolved step list. The reserve layers the
// live-emptiness test on top (complex_reserve.go). These pure fixtures pin the
// step-list half without a DB.

package dispatch

import (
	"testing"

	"shingo/protocol"
)

// splitPickups partitions complexPickups' output into TRUE sources (potentialRelay
// false — a distinct bin must be found) and potential relay re-grabs (potentialRelay
// true — a bin the order parked earlier, skipped at reserve iff that node is empty).
func splitPickups(pks []pickupNeed) (sources, relays []string) {
	for _, pk := range pks {
		if pk.potentialRelay {
			relays = append(relays, pk.step.Node)
		} else {
			sources = append(sources, pk.step.Node)
		}
	}
	return sources, relays
}

func sameOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestComplexPickups_SwapRelay pins the discriminator against the single-robot
// swap relay shape (shingo-edge BuildSingleSwapSteps, material_orders.go): 4
// pickup ACTIONS split into 2 TRUE sources (new bin, old bin) and 2 potential
// relay re-grabs (the staging re-picks). A reserve over this list finds exactly 2
// distinct bins; the 2 staging re-grabs are skipped because those nodes are empty
// at reserve (the bin hasn't relayed there yet).
func TestComplexPickups_SwapRelay(t *testing.T) {
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

	pks := complexPickups(steps)
	if len(pks) != 4 {
		t.Fatalf("complexPickups = %d pickups, want 4 (every pickup action)", len(pks))
	}
	sources, relays := splitPickups(pks)

	wantSources := []string{"src", "coreNode"}
	if !sameOrder(sources, wantSources) {
		t.Errorf("true sources = %v, want %v — 4 pickups must split to 2 distinct sources", sources, wantSources)
	}
	wantRelays := []string{"inStaging", "outStaging"}
	if !sameOrder(relays, wantRelays) {
		t.Errorf("relay re-grabs = %v, want %v", relays, wantRelays)
	}
}

// TestComplexPickups_DropoffBeforePickupIsRelay documents the discriminator's
// load-bearing edge: a pickup at N is flagged a potential relay whenever an
// EARLIER step drops at N. The reserve then treats it as an ACTUAL relay only if N
// is empty; a builder that dropped at N and picked a DIFFERENT pre-existing bin
// there (occupancy forbids two bins at one concrete node today) would hold on the
// live-emptiness check rather than silently skip. This trips if the pure flag
// stops being set.
func TestComplexPickups_DropoffBeforePickupIsRelay(t *testing.T) {
	steps := []resolvedStep{
		{Action: protocol.ActionDropoff, Node: "N"},
		{Action: protocol.ActionPickup, Node: "N"}, // flagged potential relay
	}
	pks := complexPickups(steps)
	if len(pks) != 1 {
		t.Fatalf("complexPickups = %d, want 1", len(pks))
	}
	if !pks[0].potentialRelay {
		t.Errorf("pickup after same-node dropoff: potentialRelay = false, want true")
	}
}

// TestComplexPickups_EmptyLegIsATrueSource confirms an empty-carrier pickup leg
// (produce "bring an empty to fill", Empty=true) at a real source with no prior
// dropoff is a TRUE source (potentialRelay false) — the discriminator keys on
// relay position, not payload — and carries Empty through for the reserve.
func TestComplexPickups_EmptyLegIsATrueSource(t *testing.T) {
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "emptyPool", Empty: true},
		{Action: protocol.ActionDropoff, Node: "produceLine"},
	}
	pks := complexPickups(steps)
	if len(pks) != 1 || pks[0].potentialRelay || pks[0].step.Node != "emptyPool" || !pks[0].step.Empty {
		t.Errorf("complexPickups = %+v, want a single non-relay empty-leg pickup at emptyPool", pks)
	}
}
