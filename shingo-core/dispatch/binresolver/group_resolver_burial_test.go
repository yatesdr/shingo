package binresolver

import (
	"errors"
	"fmt"
	"testing"

	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// group_resolver_burial_test.go — a lane the burial guard closed is a DIVERT,
// not a wedge.
//
// The guard lives in the slot selector (nodes.FindStoreSlotInLaneExcluding). What
// makes it safe at this level is that the resolver already treats any per-lane
// selector error as "walk on" — so a closed lane leaves the candidate set exactly
// the way a full one does, and the store lands in a sibling. That behaviour is
// pre-existing; these tests pin it against the NEW error, because "the guard
// diverts" is the claim the whole design rests on and it would be invisible if
// the sentinel ever started propagating instead of being skipped.

// claimClosedStore wraps the fake and makes named lanes answer the way a lane
// with a claimed bin deeper in it answers.
//
// Written inline here rather than added to fakeStore, per that type's own rule:
// this is a per-method error condition one test family needs, not a shape the
// fake should grow.
type claimClosedStore struct {
	*fakeStore
	closed map[int64]bool
}

// OVERRIDES THE OWNER-AWARE FORM, because that is the one the group resolvers
// call. It overrode the blind FindStoreSlotInLane until the resolvers started
// threading asker.OrderID through — at which point the wrapper stopped being
// consulted at all and every "closed" lane silently answered normally. An
// embedded-type override that the caller has stopped calling is a fixture that
// tests nothing while still passing, so the blind form now delegates here rather
// than to the fake, and there is one place to close a lane.
func (c *claimClosedStore) FindStoreSlotInLaneExcluding(laneID, excludeOrderID int64) (*nodes.Node, error) {
	if c.closed[laneID] {
		return nil, fmt.Errorf("no empty slot in lane %d: %w", laneID, nodes.ErrLaneClosedByClaim)
	}
	return c.fakeStore.FindStoreSlotInLaneExcluding(laneID, excludeOrderID)
}

func (c *claimClosedStore) FindStoreSlotInLane(laneID int64) (*nodes.Node, error) {
	return c.FindStoreSlotInLaneExcluding(laneID, 0)
}

// TestBurialDivert_ClosedLaneFallsToASibling is the property the guard is safe
// under: refusing one lane costs a walk, not a park.
//
// Both algorithms are exercised, because the two scan differently — LKND builds a
// candidate set and ranks it, DPTH returns the first lane that answers — and a
// change to either loop's error handling would break the divert in a way the
// other would hide.
func TestBurialDivert_ClosedLaneFallsToASibling(t *testing.T) {
	t.Parallel()
	for _, algo := range []string{"LKND", "DPTH"} {
		t.Run(algo, func(t *testing.T) {
			f := newFakeStore()
			group := ngrpNode(1, "grp")
			f.setProp(group.ID, "store_algorithm", algo)
			closed := laneChild(10, "closed")
			open := laneChild(11, "open")
			f.children[group.ID] = []*nodes.Node{closed, open}
			openSlot := slotInLane(101, "open-slot")
			f.storeSlot[closed.ID] = slotInLane(100, "closed-slot") // would answer, but the guard refuses
			f.storeSlot[open.ID] = openSlot

			gr := &GroupResolver{DB: &claimClosedStore{fakeStore: f, closed: map[int64]bool{closed.ID: true}}}
			got, err := gr.ResolveStore(group, "P1", nil, reservations.Anyone)
			if err != nil {
				t.Fatalf("resolve: %v — one closed lane must not fail the group; the scan walks on", err)
			}
			if got.Node != openSlot {
				t.Fatalf("slot = %s, want the sibling lane's slot. A lane closed by a claim is skipped "+
					"exactly like a full one — same disposition, different diagnosis", got.Node.Name)
			}
		})
	}
}

// TestBurialDivert_AllLanesClosedParksUnderTheExistingShape pins the other end.
//
// When every lane refuses there is nothing to divert to, and the order has to
// park. It must park under the SAME error the group has always returned when it
// has no room — "no available slot in node group X" — because the queue-reason
// classifier matches that message by substring and reads the group name as
// everything after it. A sentinel or a suffix here would either mis-classify the
// park or surface inside the operator's sentence as part of the group's name.
func TestBurialDivert_AllLanesClosedParksUnderTheExistingShape(t *testing.T) {
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

			gr := &GroupResolver{DB: &claimClosedStore{
				fakeStore: f,
				closed:    map[int64]bool{a.ID: true, b.ID: true},
			}}
			_, err := gr.ResolveStore(group, "P1", nil, reservations.Anyone)
			if err == nil {
				t.Fatal("every lane closed: the group must refuse")
			}
			if got, want := err.Error(), "no available slot in node group grp"; got != want {
				t.Fatalf("err = %q, want exactly %q — the classifier substring-matches this message "+
					"and takes the group name as everything after \"node group \", so a suffix would "+
					"land inside the operator's sentence", got, want)
			}
			if errors.Is(err, nodes.ErrLaneClosedByClaim) {
				t.Error("the per-lane sentinel must not propagate to the group error; it is a " +
					"diagnosis for the log line, not a control-flow signal for the caller")
			}
		})
	}
}
