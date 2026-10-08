//go:build docker

package dispatch

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// lane_unification_docker_test.go — the plain path asks Hold B, and appears in
// the answer.
//
// Owner ruling: "Anything going into lanes needs to follow the gate and paths.
// I don't even want a switch. It's the authority." So there is no mode gate and
// no flag below: every lane here is on the DEFAULT enforcement mode (`none`),
// which is what both plants run and the only configuration that exists.
//
// The unification is two halves and it is worth nothing as one. Asking "is
// anyone inside this lane" was already possible; before this change the answer
// only ever contained COMPOUND legs, because TakeLaneOccupancy had exactly one
// caller (compound.go). A plain order neither appeared in the answer nor
// consulted it. So these tests come in pairs: the refusal, and the row that
// makes the refusal reachable.

// occupantsOf is the durable Hold B state, read the way admission reads it.
func occupantsOf(t *testing.T, d *Dispatcher, laneID int64) []int64 {
	t.Helper()
	occ, err := reservations.OccupantsOf(d.db.DB, laneID)
	if err != nil {
		t.Fatalf("occupants of lane %d: %v", laneID, err)
	}
	return occ
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// TestUnification_PlainStoreParksOnAnOccupiedLane is the ask half.
//
// MUTATION (verified): restore `occupancy: true` to skipsForPlainEntry. This
// fires — the order is admitted into a lane a robot is standing in.
func TestUnification_PlainStoreParksOnAnOccupiedLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	laneID, shallow, _ := noneLaneWithTwoSlots(t, db, "UNI-ASK")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	line, err := db.GetNodeByDotName(sd.LineNode.Name)
	if err != nil {
		t.Fatalf("resolve line: %v", err)
	}

	inside := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "uni-ask-inside" }, asDispatched) // (C1.5: an occupant is dispatched)
	if _, err := reservations.AcquireOccupancy(db.DB, inside.ID, laneID); err != nil {
		t.Fatalf("acquire occupancy: %v", err)
	}

	// Every lane is marked: the store is sent to the mark and the gate's valve
	// refuses its entry, on the row (F2).
	store := admitOrder(t, db, "uni-ask-store", shallow)
	held := heldAtTheMark(t, d, db, store, line, shallow)
	if held.QueueCause != string(CauseLaneOccupied) {
		t.Errorf("cause = %q, want %q — the row has to say WHY, and occupied is investigated "+
			"differently from a dig or from traffic", held.QueueCause, CauseLaneOccupied)
	}
}

// TestUnification_PlainDispatchRecordsItsOwnPresence is the take half — the one
// that makes the ask mean anything.
//
// MUTATION (verified): delete the TakeLaneOccupancy call in dispatchToFleetCore.
// This fires, and so does the serialization test below — which is the point: the
// ask alone looks like it works until a second plain order needs to see the
// first.
func TestUnification_PlainDispatchRecordsItsOwnPresence(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	laneID, shallow, _ := noneLaneWithTwoSlots(t, db, "UNI-TAKE")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	line, err := db.GetNodeByDotName(sd.LineNode.Name)
	if err != nil {
		t.Fatalf("resolve line: %v", err)
	}
	if occ := occupantsOf(t, d, laneID); len(occ) != 0 {
		t.Fatalf("fixture: lane already occupied by %v", occ)
	}

	store := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "uni-take"
		o.StationID = "line-1"
		o.SourceNode = line.Name
		o.DeliveryNode = shallow.Name
		o.Status = "pending"
	})
	if _, err := d.DispatchDirect(store, line, shallow); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}

	if occ := occupantsOf(t, d, laneID); !containsID(occ, store.ID) {
		t.Fatalf("a dispatched plain store recorded no occupancy on the lane it was sent into "+
			"(occupants %v). Hold B had exactly one writer — the compound path — so the plain "+
			"path asked a question it never answered for anyone else", occ)
	}
}

// TestUnification_TwoPlainStoresIntoOneLaneSerialize is both halves at once, and
// it is the behaviour the ruling actually asks for.
//
// Neither half alone produces it: without the take the second store sees an
// empty lane, and without the ask it never looks.
func TestUnification_TwoPlainStoresIntoOneLaneSerialize(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	_, shallow, deep := noneLaneWithTwoSlots(t, db, "UNI-SER")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	line, err := db.GetNodeByDotName(sd.LineNode.Name)
	if err != nil {
		t.Fatalf("resolve line: %v", err)
	}

	first := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "uni-ser-1"
		o.StationID = "line-1"
		o.SourceNode = line.Name
		o.DeliveryNode = deep.Name
		o.Status = "pending"
	})
	// The first store passes the gate and goes in.
	if _, err := d.DispatchDirect(first, line, deep); err != nil {
		t.Fatalf("DispatchDirect first: %v", err)
	}
	if f, _ := db.GetOrder(first.ID); IsGateStaged(f) {
		t.Fatalf("the first store is held at the mark of an empty lane (cause %q) — an empty lane must admit",
			f.QueueCause)
	}

	// The second is sent to the mark and held there: two robots never share the lane.
	second := admitOrder(t, db, "uni-ser-2", shallow)
	held := heldAtTheMark(t, d, db, second, line, shallow)
	if held.QueueCause == "" {
		t.Errorf("the second store is held at the mark with no cause on its row")
	}
}

// TestUnification_PlainRetrieveParksOnAnOccupiedSourceLane — the other
// direction. A retrieve's lane is its SOURCE, and admission asks the source end
// first, so this is the same question at the other end of the order.
func TestUnification_PlainRetrieveParksOnAnOccupiedSourceLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	laneID, shallow, _ := noneLaneWithTwoSlots(t, db, "UNI-RET")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	line, err := db.GetNodeByDotName(sd.LineNode.Name)
	if err != nil {
		t.Fatalf("resolve line: %v", err)
	}
	inside := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "uni-ret-inside" }, asDispatched) // (C1.5: an occupant is dispatched)
	if _, err := reservations.AcquireOccupancy(db.DB, inside.ID, laneID); err != nil {
		t.Fatalf("acquire occupancy: %v", err)
	}

	retrieve := admitOrder(t, db, "uni-ret", shallow)
	held := heldAtTheMark(t, d, db, retrieve, shallow, line)
	if held.QueueCause != string(CauseLaneOccupied) {
		t.Errorf("retrieve out of an occupied lane is held under %q, want %q", held.QueueCause, CauseLaneOccupied)
	}
}

// TestUnification_UnoccupiedLaneIsUnaffected is the narrowness assertion: the
// change adds a refusal where a robot is inside, not a new way for ordinary work
// to park. Every plain order at both plants is this case.
func TestUnification_UnoccupiedLaneIsUnaffected(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	_, shallow, _ := noneLaneWithTwoSlots(t, db, "UNI-CLEAR")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	line, err := db.GetNodeByDotName(sd.LineNode.Name)
	if err != nil {
		t.Fatalf("resolve line: %v", err)
	}
	store := admitOrder(t, db, "uni-clear", shallow, asDispatched) // (C1.5: a lane holder is dispatched)
	admitted, cause, laneName, err := d.AcquireLanesForOrder(store, line, shallow, EntryFreshBin)
	if err != nil {
		t.Fatalf("AcquireLanesForOrder: %v", err)
	}
	if !admitted || cause != "" || laneName != "" {
		t.Fatalf("empty lane: admitted=%v cause=%q lane=%q — nothing here is gated",
			admitted, cause, laneName)
	}
}

// TestUnification_FailedDispatchLeavesNoOccupancy pins the END STATE, and the
// honest note about which mechanism delivers it.
//
// The take happens BEFORE the fleet handover so no robot is ever committed with
// its presence unrecorded. That ordering is only safe if a failed dispatch gives
// the row back — otherwise the first fleet blip wedges the lane for every later
// order, permanently, with nothing to clear it. THAT is what this asserts.
//
// MUTATION (RUN, AND IT DID NOT FIRE — recorded rather than hidden): deleting
// the ReleaseLaneOccupancy call in dispatchToFleetCore's handover-failure arm
// leaves this test GREEN. The reason is that both callers terminalize on a
// dispatch error (DispatchDirect → lifecycle.Fail, dispatchToFleet → failOrder)
// and TerminalizeOrder releases every reservation in the same transaction as the
// status write (store/orders.go → reservations.ReleaseByOrder). So on today's
// paths terminalization is the load-bearing releaser and the rollback arm is
// defence-in-depth for a caller that does not terminalize.
//
// The test is kept anyway because the PROPERTY is what the floor cares about —
// a failed dispatch must not leave a lane looking occupied — and it would catch
// a future caller that parks instead of failing. It is NOT evidence that the
// rollback arm works; nothing here is.
func TestUnification_FailedDispatchLeavesNoOccupancy(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	laneID, shallow, _ := noneLaneWithTwoSlots(t, db, "UNI-ROLL")
	// A backend that refuses the create: the order is claimed, no robot moves.
	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())

	line, err := db.GetNodeByDotName(sd.LineNode.Name)
	if err != nil {
		t.Fatalf("resolve line: %v", err)
	}
	store := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "uni-roll"
		o.StationID = "line-1"
		o.SourceNode = line.Name
		o.DeliveryNode = shallow.Name
		o.Status = "pending"
	})
	if _, err := d.DispatchDirect(store, line, shallow); err == nil {
		t.Fatal("fixture: the failing backend must refuse the create")
	}

	if occ := occupantsOf(t, d, laneID); containsID(occ, store.ID) {
		t.Fatalf("a store whose fleet create FAILED still holds occupancy (occupants %v). "+
			"No robot is in that lane and nothing else will ever release the row — every later "+
			"order into this lane parks forever", occ)
	}
}

// heldAtTheMark dispatches a plain order and requires it held at the lane's mark:
// sent to the waiting spot, gate-staged, nothing appended past the wait. That is
// how a marked lane refuses an entry; there is no refusal before dispatch.
func heldAtTheMark(t *testing.T, d *Dispatcher, db *store.DB, o *orders.Order, src, dst *nodes.Node) *orders.Order {
	t.Helper()
	if _, err := d.DispatchDirect(o, src, dst); err != nil {
		t.Fatalf("DispatchDirect %s: %v", o.EdgeUUID, err)
	}
	after, err := db.GetOrder(o.ID)
	if err != nil {
		t.Fatalf("reload %s: %v", o.EdgeUUID, err)
	}
	if !IsGateStaged(after) {
		t.Fatalf("%s went into the lane (status %s, wait index %d, cause %q) — it must be held at the mark",
			o.EdgeUUID, after.Status, after.WaitIndex, after.QueueCause)
	}
	return after
}
