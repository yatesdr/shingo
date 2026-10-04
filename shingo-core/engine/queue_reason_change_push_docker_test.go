//go:build docker

package engine

import (
	"encoding/json"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
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

	// A new cause that reads as the same sentence, under the same code: the wire
	// carries no cause, so the station has nothing new to read.
	d.SetQueueReason(o, protocol.QueueWaitingForSlot, dispatch.CauseStoreSlotContended,
		dispatch.QueueParams{Destination: "QRC-SPOT"})
	if o.QueueCause != string(dispatch.CauseStoreSlotContended) {
		t.Fatalf("queue_cause = %q, want the new cause written", o.QueueCause)
	}
	if n := len(queueUpdates(t, db, o.EdgeUUID)) - before; n != 1 {
		t.Errorf("updates after a change of cause alone = %d, want still 1", n)
	}
}

// ONE MESSAGE PER CHANGE, DRIVEN THROUGH THE BUS. Each row is a request as the
// station sends it: the intake parks the order, announces it queued, the queued
// event runs the scanner on this goroutine, and the push after the scan tells the
// station. Every order.update Core queued for the order is counted, whichever
// door sent it.
func TestQueueReason_EachIntakePathSendsOneMessage(t *testing.T) {
	t.Parallel()

	t.Run("buried at intake with nowhere to put the blocker", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
			Prefix: "QRBUR", NumSlots: 2, NumShuffles: 1, TargetSlot: 2, TargetAge: 2 * time.Hour,
		})
		// The blocker in front is somebody else's part, so the target is buried;
		// the one shuffle slot is taken, so the dig has nowhere to put it.
		for _, b := range sc.Blockers {
			testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, "QRBUR-OTHER", 50), "re-label blocker")
			testutil.MustNoErr(t, db.ConfirmBinManifest(b.ID, ""), "confirm blocker")
		}
		testdb.CreateBinAtNode(t, db, "QRBUR-OTHER", sc.ShuffleSlots[0].ID, "QRBUR-SQUAT")
		eng := newTestEngine(t, db, simulator.New())

		eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
			OrderUUID:   "qr-buried",
			PayloadCode: sc.Payload.Code,
			Quantity:    1,
			Steps: []protocol.ComplexOrderStep{
				{Action: "pickup", Node: sc.Grp.Name},
				{Action: "dropoff", Node: sc.LineNode.Name},
			},
		})

		o := testdb.RequireOrder(t, db, "qr-buried")
		if o.QueueCause != string(dispatch.CauseNoShuffleSlot) {
			t.Fatalf("queue_cause = %q (status %s), want %q — the fixture is not parked on the burial",
				o.QueueCause, o.Status, dispatch.CauseNoShuffleSlot)
		}
		if got := queueUpdates(t, db, "qr-buried"); len(got) != 1 || got[0] != o.QueueReason {
			t.Errorf("order.update messages = %q, want one carrying %q", got, o.QueueReason)
		}
	})

	t.Run("parked at intake, the scan re-parks under a new cause with the same sentence", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		sd := testdb.SetupStandardData(t, db)
		grp, _, _ := closedGroup(t, db, "QRSAME", sd.Payload.Code, 1)
		testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "QRSAME-STORED")
		eng := newTestEngine(t, db, simulator.New())

		eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), closedGroupStore("qr-same", sd, grp))

		o := testdb.RequireOrder(t, db, "qr-same")
		if o.QueueCause != string(dispatch.CauseNGRPResolve) {
			t.Fatalf("queue_cause = %q (status %s), want %q — the scan did not re-park it", o.QueueCause,
				o.Status, dispatch.CauseNGRPResolve)
		}
		if got := queueUpdates(t, db, "qr-same"); len(got) != 1 || got[0] != o.QueueReason {
			t.Errorf("order.update messages = %q, want one carrying %q", got, o.QueueReason)
		}
	})

	t.Run("parked at intake, the scan changes what the station reads", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		sd := testdb.SetupStandardData(t, db)
		grp, _, _ := closedGroup(t, db, "QRVIS", sd.Payload.Code, 1)
		testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "QRVIS-STORED")
		eng := newTestEngine(t, db, simulator.New())

		// A first request gives the plan its stored shape; the second order is
		// that row as intake writes it, parked under a wait the scan will not
		// agree with (the world moved between intake and the scan).
		eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), closedGroupStore("qr-vis-shape", sd, grp))
		shape := testdb.RequireOrder(t, db, "qr-vis-shape")
		o := &orders.Order{EdgeUUID: "qr-vis", StationID: shape.StationID, OrderType: shape.OrderType,
			Status: protocol.StatusSourcing, Quantity: 1, PayloadCode: shape.PayloadCode,
			SourceNode: shape.SourceNode, DeliveryNode: shape.DeliveryNode, StepsJSON: shape.StepsJSON,
			Coordinated: true}
		testutil.MustNoErr(t, db.CreateOrder(o), "the intake row")
		eng.Dispatcher().SetQueueReason(o, protocol.QueueWaitingForMaterial, dispatch.CauseIntakeResolve,
			dispatch.QueueParams{Payload: sd.Payload.Code})
		intakeSentence := o.QueueReason

		announceQueued(eng, o)

		after := testdb.RequireOrder(t, db, "qr-vis")
		if after.QueueReason == intakeSentence || after.QueueCode == string(protocol.QueueWaitingForMaterial) {
			t.Fatalf("the scan left the intake wait (%s, %q) — the fixture made no visible change",
				after.QueueCode, after.QueueReason)
		}
		if got := queueUpdates(t, db, "qr-vis"); len(got) != 1 || got[0] != after.QueueReason {
			t.Errorf("order.update messages = %q, want one carrying %q", got, after.QueueReason)
		}
	})
}

// closedGroupStore is a complex store from the line into a group with no room.
func closedGroupStore(uuid string, sd *testdb.StandardData, grp *nodes.Node) *protocol.ComplexOrderRequest {
	return &protocol.ComplexOrderRequest{
		OrderUUID:   uuid,
		PayloadCode: sd.Payload.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: sd.LineNode.Name},
			{Action: "dropoff", Node: grp.Name},
		},
	}
}

// announceQueued is the last line of complex intake: the order is announced
// queued through the engine's own emitter.
func announceQueued(eng *Engine, o *orders.Order) {
	(&dispatchEmitter{bus: eng.Events, engine: eng}).EmitOrderQueued(o.ID, o.EdgeUUID, o.StationID, o.PayloadCode,
		dispatch.WaitOf(o))
}
