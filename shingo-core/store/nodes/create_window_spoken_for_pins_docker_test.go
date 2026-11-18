//go:build docker

package nodes_test

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// TestCreateWindow_SpokenForSlotReadersCountTheOrder: an order inside its
// fleet create names the deep slot of a lane as its delivery and holds no row on
// it (the row as the hand leaves a group drop, whose slot stays paper). Every
// reader of "is this slot spoken for" counts it, through the delivery-node arm
// alone: the slot is not takeable, the mouth in front of it is refused as
// entombing it, and the one-read group state agrees (StoreSlot passes the deep
// slot over).
//
// Today the row reads `dispatched` with no vendor id, and those arms exclude
// only the pre-dispatch statuses. Once the claim is a column and the row stays
// at its waiting status, the arms must count an open claim as they count
// `dispatched`, or a dig parks a blocker in front of a slot a robot is about to
// be sent to (EntombsASpokenForSlotSQL's measured bubbles). A claimed pin: it
// stays green; the fixture is the row as the hand leaves it.
func TestCreateWindow_SpokenForSlotReadersCountTheOrder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	grp, mouth, deep := spokenForLane(t, db, "CWSF")
	namingOrder(t, db, "cwsf-in-create", protocol.StatusDispatched, deep)

	take, err := nodes.TakeableSlotsInGroup(db.DB, grp, 0)
	if err != nil {
		t.Fatalf("takeable slots: %v", err)
	}
	if take[deep.ID].Free {
		t.Errorf("%s reads free while an order inside its create is bound for it", deep.Name)
	}
	entomb, err := db.SlotsThatWouldEntombASpokenForSlot(grp)
	if err != nil {
		t.Fatalf("entombing slots: %v", err)
	}
	if !entomb[mouth.ID] {
		t.Errorf("%s is not refused in front of %s, which an order inside its create is bound for", mouth.Name, deep.Name)
	}
	state, err := nodes.ReadGroupSlotState(db.DB, grp)
	if err != nil {
		t.Fatalf("group slot state: %v", err)
	}
	if slot, _ := state.StoreSlot(*deep.ParentID, 0); slot == nil || slot.ID != mouth.ID {
		got := "none"
		if slot != nil {
			got = slot.Name
		}
		t.Errorf("the group state's store slot = %s, want %s (the deep slot is spoken for)", got, mouth.Name)
	}
}
