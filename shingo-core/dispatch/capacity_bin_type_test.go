package dispatch

import (
	"errors"
	"testing"

	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// capacity_bin_type_test.go — THE GATE AND THE RESOLVER MUST AGREE.
//
// checkNGRPCapacity counted a child FREE on occupancy alone, so a group whose
// only empty positions are fenced against the arriving carrier reported room.
// The resolver then refused, and the order was admitted, resolved, refused and
// parked one layer deeper under a cause that says nothing about the fence — the
// same admitted-then-parked loop the level question was given a shared read to
// avoid (see ngrpAtDeclaredLevel's comment).
//
// That divergence was harmless only while the resolver's own bin-type check was
// inert. It goes live with it, which is why these land in the same change.

func fencedNGRP() *fakeCapacityDB {
	return &fakeCapacityDB{
		node: &nodes.Node{ID: 300, Name: "SMKT", IsSynthetic: true, NodeTypeCode: "NGRP"},
		children: []*nodes.Node{
			{ID: 301, Name: "SMKT_S1", Enabled: true},
			{ID: 302, Name: "SMKT_S2", Enabled: true},
		},
		// Both slots physically empty — the whole point is that occupancy says
		// "room" and the fence says otherwise.
		binsByChild:    map[int64]int{301: 0, 302: 0},
		inFlightByName: map[string]int{"SMKT_S1": 0, "SMKT_S2": 0},
		effBinTypes: map[int64][]*bins.BinType{
			301: {{ID: 2, Code: "TOTE-2415"}},
			302: {{ID: 2, Code: "TOTE-2415"}},
		},
	}
}

// EVERY FREE SLOT FENCED = blocked, and as ngrp-full rather than a check
// failure: the group genuinely has nowhere for THIS carrier, which is a real
// capacity answer and not a read that did not complete.
func TestNGRPCapacity_AllFreeChildrenFenced_Blocks(t *testing.T) {
	t.Parallel()
	db := fencedNGRP()
	db.orderBinTypes = map[int64]int64{42: 1} // a knockdown, into a tote-only group

	blocked, block := CheckDropoffCapacity(db, "SMKT", 42)
	if !blocked {
		t.Fatal("the gate reported room in a group whose every free slot refuses " +
			"this carrier type.\n\n" +
			"The resolver will refuse it, so the order is admitted, resolved, refused " +
			"and parked a layer deeper under a cause that never mentions the fence.")
	}
	if string(block.Cause) != "ngrp-full" {
		t.Errorf("cause = %q, want ngrp-full — the group has nowhere for this carrier, "+
			"which is a capacity fact and not a read that failed", block.Cause)
	}
}

// THE MATCHING CARRIER STILL PASSES, so the case above is not just "the fence
// blocks everything".
func TestNGRPCapacity_MatchingCarrierPasses(t *testing.T) {
	t.Parallel()
	db := fencedNGRP()
	db.orderBinTypes = map[int64]int64{42: 2} // a tote, into the tote-only group

	if blocked, block := CheckDropoffCapacity(db, "SMKT", 42); blocked {
		t.Errorf("a tote was refused room in a group declaring TOTE-2415 (cause=%q)", block.Cause)
	}
}

// AN UNKNOWN CARRIER NARROWS NOTHING. Every order in the plant is here until the
// derivation can name its carrier, so this is the path that must not change.
func TestNGRPCapacity_UnknownBinTypeIgnoresFence(t *testing.T) {
	t.Parallel()
	db := fencedNGRP() // no orderBinTypes entry = "could not tell"

	if blocked, block := CheckDropoffCapacity(db, "SMKT", 42); blocked {
		t.Errorf("an order whose carrier could not be identified was refused room "+
			"(cause=%q). Unknown means do not narrow, not refuse", block.Cause)
	}
}

// A FENCE READ THAT FAILS REPORTS capacity-check-failed, not fullness. Both
// queue the order; only one sends an operator to go clear a group that has room.
func TestNGRPCapacity_FenceReadFailure_IsNotFullness(t *testing.T) {
	t.Parallel()
	db := fencedNGRP()
	db.orderBinTypes = map[int64]int64{42: 2}
	db.effBinTypesErr = errors.New("connection reset by peer")

	blocked, block := CheckDropoffCapacity(db, "SMKT", 42)
	if !blocked {
		t.Fatal("a group whose bin-type declarations could not be read reported room")
	}
	if string(block.Cause) != "capacity-check-failed" {
		t.Errorf("cause = %q, want capacity-check-failed — nothing was learned about "+
			"these slots, and telling an operator the group is full sends them to "+
			"clear one that may be empty", block.Cause)
	}
}
