package binresolver

import (
	"testing"

	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// TestResolveStore_ResolveAround_PrefersCompatibleLane proves the arm end-to-end
// through resolveStoreLKND: with resolve_around ON, the store ranker prefers a
// mouth-compatible lane among equal-depth candidates — even when the blocked lane
// is emptier (which wins on the count tiebreak when the arm is OFF). This pins
// both the flip (ON) and byte-identical behavior (OFF), and that the arm is gated
// strictly on the group property.
func TestResolveStore_ResolveAround_PrefersCompatibleLane(t *testing.T) {
	t.Parallel()

	// build wires two equal-depth lanes (laneChild leaves Depth nil → depth 0):
	// the "blocked" lane is emptier but its mouth is held in a conflicting mode;
	// the "free" lane is fuller but open. Returns the slot the ranker picks.
	build := func(resolveAroundOn bool) *nodes.Node {
		f := newFakeStore()
		group := ngrpNode(1, "grp")
		blocked := laneChild(10, "lane-blocked")
		free := laneChild(11, "lane-free")
		f.children[group.ID] = []*nodes.Node{blocked, free}

		slotBlocked := slotInLane(100, "BLOCKED-1")
		slotFree := slotInLane(101, "FREE-1")
		f.storeSlot[blocked.ID] = slotBlocked
		f.storeSlot[free.ID] = slotFree

		// Emptiest wins on the count tiebreak with the arm off — make the blocked
		// lane the emptier one, so a flip to the free lane can only come from the
		// resolve-around compatibility key.
		f.laneBinCounts[blocked.ID] = 0
		f.laneBinCounts[free.ID] = 3

		// Mouth state: blocked lane held in a conflicting mode; free lane open.
		f.laneAccepts[blocked.ID] = false
		f.laneAccepts[free.ID] = true

		if resolveAroundOn {
			f.setProp(group.ID, PropResolveAround, "on")
		}

		gr := &GroupResolver{DB: f}
		res, err := gr.ResolveStore(group, "", nil, reservations.Anyone)
		if err != nil {
			t.Fatalf("ResolveStore: %v", err)
		}
		return res.Node
	}

	// OFF: emptiest lane wins — byte-identical to the pre-arm ranker.
	if got := build(false); got.Name != "BLOCKED-1" {
		t.Errorf("arm OFF: got %q, want BLOCKED-1 (emptiest lane wins, unchanged)", got.Name)
	}
	// ON: the compatible lane wins the equal-depth tie despite being fuller.
	if got := build(true); got.Name != "FREE-1" {
		t.Errorf("arm ON: got %q, want FREE-1 (mouth-compatible lane wins)", got.Name)
	}
}
