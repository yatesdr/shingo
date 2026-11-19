//go:build docker

package nodes_test

import (
	"fmt"
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
// As built before, the row read `dispatched` with no vendor id. Now it rests at
// its waiting status with its ask open, and the arms count an open ask as they
// count an order the fleet has (orders.PastPreDispatchSQL), or a dig parks a blocker in
// front of a slot a robot is about to be sent to (EntombsASpokenForSlotSQL's
// measured bubbles). Each reader is asked of an order the fleet has, of a `queued` row with
// an ask open, and of a `queued` row at rest, which only names the slot and so
// does not hold it (TestSlotSpokenFor_AParkedOrderThatOnlyNamesASlotDoesNotHoldIt).
// A claimed pin: it never changed.
func TestCreateWindow_SpokenForSlotReadersCountTheOrder(t *testing.T) {
	t.Parallel()
	testdb.ExpectAskLeftOpen(t, "the pin writes an ask onto a row to ask the slot readers about it")
	db := testdb.Open(t)
	for i, row := range []struct {
		state     string
		status    protocol.Status
		asked     string
		spokenFor bool
	}{
		{"an order the fleet has", protocol.StatusDispatched, "", true},
		{"queued with an ask open", protocol.StatusQueued, "sg-cwsf-ask", true},
		{"queued at rest", protocol.StatusQueued, "", false},
	} {
		grp, mouth, deep := spokenForLane(t, db, fmt.Sprintf("CWSF%d", i))
		status := row.status
		if status == protocol.StatusDispatched {
			status = protocol.StatusPending // born resting, then the fleet's yes below
		}
		id := namingOrder(t, db, fmt.Sprintf("cwsf-%d", i), status, deep)
		job := ""
		if row.status == protocol.StatusDispatched {
			job = fmt.Sprintf("sg-cwsf-%d", i)
		}
		if _, err := db.Exec(`UPDATE orders SET status=$2, asked_vendor_order_id=$3, vendor_order_id=$4 WHERE id=$1`,
			id, string(row.status), row.asked, job); err != nil {
			t.Fatalf("%s: write the ask: %v", row.state, err)
		}

		take, err := nodes.TakeableSlotsInGroup(db.DB, grp, 0)
		if err != nil {
			t.Fatalf("takeable slots: %v", err)
		}
		if got := !take[deep.ID].Free; got != row.spokenFor {
			t.Errorf("%s: %s reads spoken for %v, want %v", row.state, deep.Name, got, row.spokenFor)
		}
		entomb, err := db.SlotsThatWouldEntombASpokenForSlot(grp)
		if err != nil {
			t.Fatalf("entombing slots: %v", err)
		}
		if got := entomb[mouth.ID]; got != row.spokenFor {
			t.Errorf("%s: %s refused as entombing %s = %v, want %v", row.state, mouth.Name, deep.Name, got, row.spokenFor)
		}
		state, err := nodes.ReadGroupSlotState(db.DB, grp)
		if err != nil {
			t.Fatalf("group slot state: %v", err)
		}
		want := deep.ID
		if row.spokenFor {
			want = mouth.ID // the deep slot is passed over
		}
		if slot, _ := state.StoreSlot(*deep.ParentID, 0); slot == nil || slot.ID != want {
			got := "none"
			if slot != nil {
				got = slot.Name
			}
			t.Errorf("%s: the group state's store slot = %s, want node %d", row.state, got, want)
		}
	}
}
