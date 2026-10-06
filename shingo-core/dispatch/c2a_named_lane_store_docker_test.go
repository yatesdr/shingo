//go:build docker

package dispatch

import (
	"testing"

	"shingocore/dispatch/binresolver"
	"shingocore/domain"
)

// TestC2aPin_ANamedLaneStoreTakesTheReachableDeepestSlot — GREEN since C2a A.3
// (red at 660fd228: into an empty lane it chose the mouth, S1).
//
// A store aimed at a LANE by name resolved through binresolver.resolveStore over
// the lane's slots (resolver.go, the ResolveModeStore arm), which checks enabled,
// the bin-type gate, bins + inbound and the slot book, and NOTHING about depth,
// reachability or burial. The lane arm reached through a group uses
// FindStoreSlotInLaneExcluding: the deepest empty slot a robot can reach from the
// mouth. Two shapes, both on one three-deep lane (S1 the mouth, S3 the deepest):
//
//   - a bin in S2: S3 is behind it and unreachable; the only lawful store is S1;
//   - the lane empty: back-to-front packing puts the bin in S3; a store at S1 or
//     S2 seals the slots behind it.
//
// Flat bug (keep-staged handoff, "a store aimed at a lane by name has no depth or
// reachability check"), fixed by sending the named-lane store through the lane
// arm's selector (GroupResolver.ResolveStoreInLane).
func TestC2aPin_ANamedLaneStoreTakesTheReachableDeepestSlot(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	r := &binresolver.DefaultResolver{DB: db}

	// Shape 1: a bin in the middle slot.
	_, walled, _, _, wSlots, _, bp := setupDwellGroup(t, db, "C2AS1", 0, false)
	createTestBinAtNode(t, db, bp.Code, wSlots[1].ID, "C2AS1-MID")
	res, err := r.Resolve(walled, binresolver.ResolveModeStore, bp.Code, binresolver.NoBinType, domain.Anyone, nil)
	if err != nil {
		t.Fatalf("a named-lane store with a lawful slot refused: %v", err)
	}
	if res.Node.ID != wSlots[0].ID {
		t.Errorf("with a bin in %s, a named-lane store chose %s; the only reachable slot is %s",
			wSlots[1].Name, res.Node.Name, wSlots[0].Name)
	}

	// Shape 2: the lane empty.
	_, empty, _, _, eSlots, _, bp2 := setupDwellGroup(t, db, "C2AS2", 0, false)
	res, err = r.Resolve(empty, binresolver.ResolveModeStore, bp2.Code, binresolver.NoBinType, domain.Anyone, nil)
	if err != nil {
		t.Fatalf("a named-lane store into an empty lane refused: %v", err)
	}
	if res.Node.ID != eSlots[2].ID {
		t.Errorf("into an empty lane, a named-lane store chose %s; back-to-front packing is %s",
			res.Node.Name, eSlots[2].Name)
	}
}
