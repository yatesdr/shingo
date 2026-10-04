//go:build docker

package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// queueUpdates returns the queue sentences of every order.update in the outbox
// for one order, in send order.
func queueUpdates(t *testing.T, db *store.DB, orderUUID string) []string {
	t.Helper()
	rows, err := db.DB.Query(`SELECT payload FROM outbox WHERE msg_type = $1 ORDER BY id`, protocol.TypeOrderUpdate)
	testutil.MustNoErr(t, err, "read outbox")
	defer rows.Close()
	var out []string
	for rows.Next() {
		var payload []byte
		if rows.Scan(&payload) != nil {
			continue
		}
		var env struct {
			P protocol.OrderUpdate `json:"p"`
		}
		if json.Unmarshal(payload, &env) == nil && env.P.OrderUUID == orderUUID {
			out = append(out, env.P.QueueReason)
		}
	}
	return out
}

// A WAIT THAT CHANGES ITS CAUSE TELLS THE STATION, ONCE. The station learned a
// queued order's sentence only when the order entered the queue; when Core's
// reason for the wait changed later (a refill waiting for material that is
// then waiting for its slot), the station went on reading the first sentence,
// so a dry market and a wrong spare standing on the spot read the same there.
func TestQueueReason_AChangedCauseIsPushedOnce(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	o := &orders.Order{EdgeUUID: "qrc-refill", StationID: "edge.test", OrderType: "retrieve",
		Status: protocol.StatusQueued, Quantity: 1, PayloadCode: "QRC-P", DeliveryNode: "QRC-SPOT"}
	testutil.MustNoErr(t, db.CreateOrder(o), "a queued refill")
	d := eng.dispatcher

	// The wait it entered the queue with: pushed by the queued event, not here.
	d.SetQueueReason(o, protocol.QueueWaitingForMaterial, dispatch.CauseReserveHolding,
		dispatch.QueueParams{Payload: "QRC-P"})
	before := len(queueUpdates(t, db, o.EdgeUUID))

	// The cause changes: the material is there, the slot is not.
	d.SetQueueReason(o, protocol.QueueWaitingForSlot, dispatch.CauseDropoffOccupied,
		dispatch.QueueParams{Destination: "QRC-SPOT"})
	got := queueUpdates(t, db, o.EdgeUUID)[before:]
	if len(got) != 1 || got[0] != o.QueueReason {
		t.Fatalf("updates after the cause changed = %q, want one carrying %q", got, o.QueueReason)
	}

	// The same wait again, every pass: nothing more.
	for i := 0; i < 3; i++ {
		d.SetQueueReason(o, protocol.QueueWaitingForSlot, dispatch.CauseDropoffOccupied,
			dispatch.QueueParams{Destination: "QRC-SPOT"})
	}
	if n := len(queueUpdates(t, db, o.EdgeUUID)) - before; n != 1 {
		t.Errorf("updates after three passes with the same wait = %d, want still 1", n)
	}
}

// ONE MESSAGE PER CHANGE, WHICHEVER DOOR SENDS IT. A cause that changes as the
// order is announced queued (a buried pickup parked at intake) used to reach the
// station twice: once from the changed wait, once from the queued event, with
// the same status and the same sentence. The second is the same message.
func TestQueueReason_AChangeAnnouncedAsQueuedIsSentOnce(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	o := &orders.Order{EdgeUUID: "qrc-intake", StationID: "edge.test", OrderType: "complex",
		Status: protocol.StatusQueued, Quantity: 1, PayloadCode: "QRC-P"}
	testutil.MustNoErr(t, db.CreateOrder(o), "a queued order")
	d := eng.dispatcher
	d.SetQueueReason(o, protocol.QueueWaitingForMaterial, dispatch.CauseReserveHolding,
		dispatch.QueueParams{Payload: "QRC-P"})
	before := len(queueUpdates(t, db, o.EdgeUUID))

	// The intake parks it under a new cause, then announces it queued.
	d.SetQueueReason(o, protocol.QueueStorageRearranging, dispatch.CauseLaneOccupied,
		dispatch.QueueParams{Lane: "QRC-LANE", Payload: "QRC-P"})
	eng.pushQueueReason(o.ID, o.EdgeUUID, o.StationID) // the queued event's push

	if got := queueUpdates(t, db, o.EdgeUUID)[before:]; len(got) != 1 {
		t.Errorf("updates for one change announced as queued = %q, want exactly one", got)
	}
}
