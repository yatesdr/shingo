package binresolver

import (
	"testing"

	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// group_resolver_dig_divert_test.go — a store never picks a lane a dig owns.
//
// Admission refuses that store anyway, which is the safety net and is not in
// question. The point of diverting at SELECTION is that a refusal at admission is
// a PARK: the order sits out the whole excavation, which is minutes, while a
// sibling lane stood empty the entire time. Choosing the sibling costs nothing
// and the order never waits at all.
//
// The mechanism is ListChildNodesUnlocked — the candidate read itself filters
// dig-held lanes in SQL, so a locked lane is never a candidate rather than being
// one every scan loop has to remember to skip. These tests pin that the STORE
// paths use it. The retrieve side has had its own coverage since the filter
// landed (TestFIFO_SkipsLockedLanes); the store side had none, which is the half
// that parks a robot behind an excavation.
//
// MUTATION for all three (verified): change resolveStoreLKND and
// resolveStoreDPTH to call ListChildNodes instead of ListChildNodesUnlocked. The
// divert tests go red — the store picks the dig's lane and would park behind it.

// TestDigDivert_StoreSkipsALockedLane is the case the ruling is about: a lane is
// being dug, a sibling is free, and the store must not wait.
//
// Both algorithms, because they scan differently — LKND builds a candidate set
// and ranks it, DPTH returns the first lane that answers — and a candidate read
// swapped in one would be invisible in the other.
func TestDigDivert_StoreSkipsALockedLane(t *testing.T) {
	t.Parallel()
	for _, algo := range []string{"LKND", "DPTH"} {
		t.Run(algo, func(t *testing.T) {
			f := newFakeStore()
			group := ngrpNode(1, "grp")
			f.setProp(group.ID, "store_algorithm", algo)
			dug := laneChild(10, "dug")
			free := laneChild(11, "free")
			f.children[group.ID] = []*nodes.Node{dug, free}

			// The dug lane has the slot both algorithms would otherwise prefer: it
			// is first in the candidate order, so a scan that saw it would take it.
			f.storeSlot[dug.ID] = slotInLane(100, "dug-slot")
			freeSlot := slotInLane(101, "free-slot")
			f.storeSlot[free.ID] = freeSlot

			f.lockLaneForDig(dug.ID)

			gr := &GroupResolver{DB: f}
			got, err := gr.ResolveStore(group, "P1", nil, reservations.Anyone)
			if err != nil {
				t.Fatalf("resolve: %v — a dug lane must cost a walk, not the group", err)
			}
			if got.Node != freeSlot {
				t.Fatalf("slot = %s, want the free lane's slot. Picking the dug lane means admission "+
					"refuses at dispatch and the store parks for the whole excavation, with a sibling "+
					"lane empty beside it the entire time", got.Node.Name)
			}
		})
	}
}

// TestDigDivert_AllLanesLockedParksUnderTheExistingShape is the other end: when
// there is nowhere to divert TO, the order parks — and it must park through the
// group's existing "no room" refusal rather than a new shape.
//
// The wording is load-bearing: the queue-reason classifier substring-matches this
// message and reads the group name as everything after "node group ". This is the
// same constraint the burial divert is pinned against, restated here because the
// two diverts reach the same exit by different routes and either could drift.
func TestDigDivert_AllLanesLockedParksUnderTheExistingShape(t *testing.T) {
	t.Parallel()
	for _, algo := range []string{"LKND", "DPTH"} {
		t.Run(algo, func(t *testing.T) {
			f := newFakeStore()
			group := ngrpNode(1, "grp")
			f.setProp(group.ID, "store_algorithm", algo)
			a := laneChild(10, "a")
			b := laneChild(11, "b")
			f.children[group.ID] = []*nodes.Node{a, b}
			f.storeSlot[a.ID] = slotInLane(100, "a-slot")
			f.storeSlot[b.ID] = slotInLane(101, "b-slot")

			f.lockLaneForDig(a.ID)
			f.lockLaneForDig(b.ID)

			gr := &GroupResolver{DB: f}
			if _, err := gr.ResolveStore(group, "P1", nil, reservations.Anyone); err == nil {
				t.Fatal("every lane dug: the group must refuse so the caller parks")
			} else if got, want := err.Error(), "no available slot in node group grp"; got != want {
				t.Fatalf("err = %q, want exactly %q — the classifier substring-matches this and takes "+
					"the group name as everything after \"node group \"", got, want)
			}
		})
	}
}

// TestDigDivert_DirectChildrenAreNotLanesAndAreNotSkipped is the narrowness
// assertion, and the mistake it guards against is a plausible one.
//
// The filter is on the dig MOUTH ROW, which is written against a lane. A direct
// physical child of the group is not a lane, cannot hold a dig row, and must stay
// a candidate — a filter written as "skip anything with a reservation" would take
// the group's parking out of service every time any lane in it was dug, which is
// exactly when the parking is needed most.
func TestDigDivert_DirectChildrenAreNotLanesAndAreNotSkipped(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	f.setProp(group.ID, "store_algorithm", "DPTH")
	dug := laneChild(10, "dug")
	parking := directChild(12, "parking")
	f.children[group.ID] = []*nodes.Node{dug, parking}
	f.storeSlot[dug.ID] = slotInLane(100, "dug-slot")
	f.lockLaneForDig(dug.ID)

	gr := &GroupResolver{DB: f}
	got, err := gr.ResolveStore(group, "P1", nil, reservations.Anyone)
	if err != nil {
		t.Fatalf("resolve: %v — the group still has its parking, which no dig can hold", err)
	}
	if got.Node != parking {
		t.Fatalf("node = %s, want the direct child %s", got.Node.Name, parking.Name)
	}
}
