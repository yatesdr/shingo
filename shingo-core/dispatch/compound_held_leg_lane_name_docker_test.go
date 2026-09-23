//go:build docker

package dispatch

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/reservations"
)

// A HELD COMPOUND LEG'S SENTENCE NAMES THE LANE THAT REFUSED IT.
//
// AdvanceCompoundOrder's refusal arm used to write QueueParams{Lane: destName},
// so the sentence named the leg's DELIVERY node as the lane being rearranged,
// whichever lane actually refused. For a U1 whose source was buried, the
// reshuffle's last leg delivers to the unloader window, and the B5 board read
// "Rearranging lane HLU_S1 to reach this material" for a loader window. Pinned
// at e83cccd1 (c82c8654) as "Rearranging lane LINE1-IN …". The verdict carries
// the refusing lane (GateVerdict.Lane), and that is the name now.
//
// The leg picks from a lane a foreign order occupies and delivers to LINE1-IN,
// a line node that is no lane at all.
func TestCompound_HeldLegSentence_NamesTheRefusingLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	parent, children, lane, _ := twoLegCompound(t, db, "LANENAME")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	foreign := digHolder(t, db, "LANENAME-foreign-occupant")
	if _, err := reservations.AcquireOccupancy(db.DB, foreign.ID, lane); err != nil {
		t.Fatalf("foreign occupancy: %v", err)
	}
	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance (held): %v", err)
	}

	held, err := db.GetOrder(children[0].ID)
	if err != nil {
		t.Fatalf("get held leg: %v", err)
	}
	laneNode, err := db.GetNode(lane)
	if err != nil {
		t.Fatalf("get lane: %v", err)
	}
	if held.QueueCause != string(CauseLaneOccupied) {
		t.Fatalf("queue_cause = %q, want %q", held.QueueCause, CauseLaneOccupied)
	}
	want := "Rearranging lane " + laneNode.Name + " to reach this material"
	if held.QueueReason != want {
		t.Errorf("queue_reason = %q, want %q — the lane that refused, not the leg's destination %s",
			held.QueueReason, want, held.DeliveryNode)
	}
}
