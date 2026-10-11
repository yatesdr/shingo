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
	"shingocore/store/orders"
)

// c1_ladder_pins_docker_test.go — C1's ladder, pinned on a started engine
// (RULINGS-C1-P0-2026-10-02 F1, F2; PLAN row C1, the walk-path pin).

// historyOf is an order's status walk: one entry per order_history row, status
// and code, oldest first.
func historyOf(t *testing.T, db *store.DB, orderID int64) []string {
	t.Helper()
	rows, err := db.DB.Query(`SELECT status, COALESCE(code, '') FROM order_history WHERE order_id = $1 ORDER BY id`, orderID)
	testutil.MustNoErr(t, err, "read history")
	defer rows.Close()
	var out []string
	for rows.Next() {
		var status, code string
		testutil.MustNoErr(t, rows.Scan(&status, &code), "scan history")
		if code != "" {
			status += "(" + code + ")"
		}
		out = append(out, status)
	}
	return out
}

func envelopeWithUOP(t *testing.T, uop int) *protocol.Envelope {
	t.Helper()
	body, err := json.Marshal(&protocol.OrderRequest{RemainingUOP: &uop})
	testutil.MustNoErr(t, err, "marshal request body")
	payload, err := json.Marshal(protocol.Data{Body: body})
	testutil.MustNoErr(t, err, "marshal data wrapper")
	env := testEnvelope()
	env.Payload = payload
	return env
}

// TestScanner_NeverAcquiresAHalfAppliedBirth (F1). Every order is born
// `pending`, and `pending` is in the scan set, so a pass can reach a wire order
// between its INSERT and its apply: a move with no remaining_uop (the manifest
// sync skipped), a destination still naming its group, a dig pivot raced. The
// door holds the scanner's lock across its birth, its apply and its one acquire,
// so no pass runs inside it.
//
// The seam pauses the door after its INSERT and starts a pass. The pass must not
// finish while the door is paused.
//
// RED at 647b65af: the door holds no lock, the pass runs to its end inside the
// pause. (The half-applied order is not acquired there only because `pending`
// is outside the base's scan set.)
func TestScanner_NeverAcquiresAHalfAppliedBirth(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storage, line, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storage.ID, "HALF-BIRTH-BIN")
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)
	d := eng.Dispatcher()

	passDone := make(chan struct{})
	var fired, passRanInside bool
	d.SetPostBirthHook(func(int64) {
		d.SetPostBirthHook(nil)
		fired = true
		go func() {
			eng.RunFulfillmentScan()
			close(passDone)
		}()
		select {
		case <-passDone:
			passRanInside = true
		case <-time.After(500 * time.Millisecond):
		}
	})

	d.HandleOrderRequest(envelopeWithUOP(t, 7), &protocol.OrderRequest{
		OrderUUID:    "half-birth",
		OrderType:    dispatch.OrderTypeMove,
		PayloadCode:  bp.Code,
		SourceNode:   storage.Name,
		DeliveryNode: line.Name,
		Quantity:     1,
	})
	if !fired {
		t.Fatal("fixture: the post-birth seam never fired")
	}
	select {
	case <-passDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the pass never finished after the door let go of the lock")
	}
	if passRanInside {
		t.Fatal("a scanner pass ran to its end while the wire door held a row it had not finished " +
			"applying: the door does not hold the scan lock across its birth, apply and acquire")
	}
	got := testdb.RequireOrder(t, db, "half-birth")
	if got.Status != protocol.StatusDispatched || got.RemainingUOP == nil || *got.RemainingUOP != 7 {
		t.Fatalf("order is %s with remaining_uop %v, want dispatched carrying 7", got.Status, got.RemainingUOP)
	}
	if got.BinID == nil || *got.BinID != bin.ID {
		t.Fatalf("order carries bin %v, want %d", got.BinID, bin.ID)
	}
}

// TestLadder_WalkPath (PLAN row C1): an order whose first hand fails walks
// pending → queued(cause) → dispatched, one history row per edge, none for
// `sourcing`.
//
// The first hand is refused at its bin leg: the pre-hand seam hard-claims the
// order's bin for another order, so the hand's claim of it fails and the order
// parks under its leg's cause, keeping its soft hold. The claim is then taken
// back and a pass re-asks.
//
// RED at 647b65af: the walk is pending, queued with no cause (the queue tail's
// "awaiting inventory"), sourcing, queued(cause), sourcing, dispatched.
func TestLadder_WalkPath(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storage, line, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, storage.ID, "WALK-BIN")
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "walk-holder"
		o.Status = protocol.StatusDispatched
	})
	eng := newTestEngine(t, db, simulator.New())
	d := eng.Dispatcher()

	refused := false
	d.SetPreHandHook(func(int64) {
		if refused {
			return
		}
		refused = true
		_, err := db.DB.Exec(`UPDATE bins SET claimed_by = $1 WHERE id = $2`, holder.ID, bin.ID)
		testutil.MustNoErr(t, err, "hook: claim the bin for the holder")
	})

	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "walk",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: line.Name,
		Quantity:     1,
	})
	if !refused {
		t.Fatal("fixture: no hand was asked for the order")
	}
	o := testdb.RequireOrder(t, db, "walk")
	if o.Status != protocol.StatusQueued || o.QueueCode == "" {
		t.Fatalf("after a refused first hand the order is %s with code %q, want queued with its cause", o.Status, o.QueueCode)
	}

	_, err := db.DB.Exec(`UPDATE bins SET claimed_by = NULL WHERE id = $1`, bin.ID)
	testutil.MustNoErr(t, err, "give the bin back")
	eng.RunFulfillmentScan()

	o = testdb.RequireOrder(t, db, "walk")
	walk := historyOf(t, db, o.ID)
	want := []string{"pending", "queued(" + string(protocol.QueueWaitingForMaterial) + ")", "dispatched"}
	if o.Status != protocol.StatusDispatched || len(walk) != len(want) {
		t.Fatalf("status %s, walk %v; want dispatched, walk %v", o.Status, walk, want)
	}
	for i := range want {
		if walk[i] != want[i] {
			t.Fatalf("walk %v, want %v", walk, want)
		}
	}
}

// TestResume_TheParentGoesBackToPendingAndAcquiresOnce (F2): a resumed compound
// parent is not waiting on anything, so it does not say `queued`. It returns to
// the ladder's start and gets its one acquire at once, landing on `dispatched` or
// on `queued` with the cause its park wrote; the station is told where it
// landed, never `pending`.
//
// The fixture parent (main's TestResumeCompound_KeepsTheWaitTheScanWrites) parks
// again on its acquire, under ngrp-resolve.
//
// RED at 647b65af: reshuffling → queued with no cause on the row, then the scan
// writes one.
func TestResume_TheParentGoesBackToPendingAndAcquiresOnce(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	grp, _, _ := closedGroup(t, db, "C1RES", sd.Payload.Code, 1)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "C1RES-STORED")
	eng := newTestEngine(t, db, simulator.New())

	eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), closedGroupStore("c1res-shape", sd, grp))
	shape := testdb.RequireOrder(t, db, "c1res-shape")
	parent := &orders.Order{EdgeUUID: "c1res-parent", StationID: shape.StationID, OrderType: shape.OrderType,
		Status: protocol.StatusReshuffling, Quantity: 1, PayloadCode: shape.PayloadCode,
		SourceNode: shape.SourceNode, DeliveryNode: shape.DeliveryNode, StepsJSON: shape.StepsJSON,
		Coordinated: true}
	testutil.MustNoErr(t, db.CreateOrder(parent), "the parent in reshuffling")

	testutil.MustNoErr(t, eng.Dispatcher().Lifecycle().ResumeCompound(parent), "resume the parent")

	after := testdb.RequireOrder(t, db, "c1res-parent")
	walk := historyOf(t, db, parent.ID)
	want := []string{"reshuffling", "pending", "queued(" + string(protocol.QueueWaitingForSlot) + ")"}
	if len(walk) != len(want) {
		t.Fatalf("walk %v, want %v", walk, want)
	}
	for i := range want {
		if walk[i] != want[i] {
			t.Fatalf("walk %v, want %v", walk, want)
		}
	}
	if after.Status != protocol.StatusQueued || after.QueueCause != string(dispatch.CauseNGRPResolve) {
		t.Fatalf("the resumed parent is %s under %q, want queued under %q", after.Status, after.QueueCause,
			dispatch.CauseNGRPResolve)
	}
	for _, st := range statusUpdates(t, db, parent.EdgeUUID) {
		if st != string(protocol.StatusQueued) {
			t.Errorf("the station was told %q; a resumed parent that parks again is told queued, once", st)
		}
	}
	if n := len(statusUpdates(t, db, parent.EdgeUUID)); n != 1 {
		t.Errorf("the station was sent %d status updates for the resumed parent, want 1 (where it landed)", n)
	}
}

// statusUpdates lists the statuses of the order.update messages sent for one
// order, oldest first.
func statusUpdates(t *testing.T, db *store.DB, orderUUID string) []string {
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
			out = append(out, env.P.Status)
		}
	}
	return out
}

// TestRecovery_APendingOnDeckOrderIsNotParked (census delta §2): a recovery
// order superseded by a second press is one that WAITS — queued, never handed to
// the fleet. A `pending` one is mid-door, about to acquire, and refuses the
// press as in flight. Characterization: the base answers the same.
func TestRecovery_APendingOnDeckOrderIsNotParked(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		status protocol.Status
		vendor string
		parked bool
	}{
		{protocol.StatusPending, "", false},
		{protocol.StatusQueued, "", true},
		{protocol.StatusDispatched, "V-1", false},
	} {
		if got := isParkedOnDeckOrder(&orders.Order{Status: c.status, VendorOrderID: c.vendor}); got != c.parked {
			t.Errorf("isParkedOnDeckOrder(%s, vendor %q) = %v, want %v", c.status, c.vendor, got, c.parked)
		}
	}
}
