//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// holdShape is what an order's paper looks like at a moment: one entry per
// reservation, as kind/state, plus what it hard-claims.
type holdShape struct {
	res      []string // "bin:pending", "slot:confirmed", "mouth:confirmed", ...
	binsOwn  int
	slotsOwn int
}

func shapeOf(t *testing.T, db *store.DB, orderID int64) holdShape {
	t.Helper()
	var out holdShape
	rows, err := db.DB.Query(
		`SELECT resource_kind, state FROM reservations WHERE order_id=$1 ORDER BY resource_kind, id`, orderID)
	testutil.MustNoErr(t, err, "read reservations")
	defer rows.Close()
	for rows.Next() {
		var kind, state string
		testutil.MustNoErr(t, rows.Scan(&kind, &state), "scan reservation")
		out.res = append(out.res, kind+":"+state)
	}
	testutil.MustNoErr(t, rows.Err(), "reservations rows")
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT COUNT(*) FROM bins WHERE claimed_by=$1`, orderID).Scan(&out.binsOwn), "count bin claims")
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT COUNT(*) FROM nodes WHERE claimed_by=$1`, orderID).Scan(&out.slotsOwn), "count slot claims")
	return out
}

func junctionRows(t *testing.T, db *store.DB, orderID int64) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT COUNT(*) FROM order_bins WHERE order_id=$1`, orderID).Scan(&n), "count order_bins")
	return n
}

// armedOrderAwaitingFleet builds the state an order is in the instant before the
// fleet is asked: sourcing, its bin pointed at and hard-claimed, its bin
// reservation confirmed. That is what a fleet refusal has to undo.
func armedOrderAwaitingFleet(t *testing.T, db *store.DB, d *Dispatcher, uuid string) (*orders.Order, int64) {
	t.Helper()
	srcNode, lineNode, bp := setupTestData(t, db)
	bin := testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "DEMOTE-"+uuid)
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.SourceNode = srcNode.Name
		o.DeliveryNode = lineNode.Name
		o.Status = StatusSourcing
	})
	// The soft hold the scanner takes, then the pointer, then the confirm — the
	// same order the plain path runs them in.
	testutil.MustNoErr(t, d.binManifest.ReserveForDispatch(bin.ID, order.ID), "soft-reserve the bin")
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, bin.ID), "stamp the bin")
	order, _ = db.GetOrder(order.ID)
	testutil.MustNoErr(t, d.ConfirmForDispatch(order, bin.ID, srcNode, lineNode), "confirm at dispatch")
	order, _ = db.GetOrder(order.ID)
	return order, bin.ID
}

// TestFleetRefusal_TheOrderKeepsItsPaperAndRetries is the blip, and it is ruling
// §8 in one test: "this is a blip failure — everything fired, it got all its
// claims, the failure just landed with RDS."
//
// The rollback this replaced released the armor AND DELETED the reservation
// while leaving orders.bin_id stamped. That is the pointer wedge: the order
// re-enters through dispatchHeldBin, which confirms by id and never re-acquires,
// and the confirm underneath requires the pending reservation that was just
// deleted. It parked under claim-failed and retried forever, alive so no sweep
// touched it.
//
// The undo removes only what claimed a robot that never came. The paper is
// DEMOTED confirmed→pending, never deleted; the pointer and the junction rows
// stay; and on a blip the order re-wins its own uncontested bin seconds later.
func TestFleetRefusal_TheOrderKeepsItsPaperAndRetries(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	refusing, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())
	order, binID := armedOrderAwaitingFleet(t, db, refusing, "demote-blip")

	before := shapeOf(t, db, order.ID)
	if before.binsOwn != 1 {
		t.Fatalf("the order does not hold its bin claim before the refusal (%+v) — the fixture is "+
			"not armed and the rest of this test proves nothing", before)
	}

	srcNode, lineNode, _ := setupTestData(t, db)
	derr := func() error {
		_, err := refusing.DispatchDirect(order, srcNode, lineNode)
		return err
	}()
	if derr == nil {
		t.Fatal("the fleet refused the create; DispatchDirect must report it")
	}
	// The plain path in full: DispatchDirect undoes its own CAS with no sentence,
	// and the caller — which is what knows whether this is a wait or the end of a
	// request — names it. This is the scanner's half.
	order, _ = db.GetOrder(order.ID)
	code, cause, params := FleetRefusalCause(derr, order.DeliveryNode)
	refusing.DemoteAfterFleetRefusal(order, code, cause, params)

	got, gerr := db.GetOrder(order.ID)
	testutil.MustNoErr(t, gerr, "re-read the order")

	if got.Status != StatusSourcing {
		t.Errorf("status = %q, want %q — a fleet refusal is congestion, and the order stays alive "+
			"to be retried", got.Status, StatusSourcing)
	}
	if got.QueueCause != string(CauseFleetRefusedCreate) {
		t.Errorf("queue_cause = %q, want %q", got.QueueCause, CauseFleetRefusedCreate)
	}
	if got.BinID == nil || *got.BinID != binID {
		t.Errorf("orders.bin_id = %v, want %d kept — the bin is still spoken for; §8 keeps the pointer",
			got.BinID, binID)
	}

	after := shapeOf(t, db, order.ID)
	if after.binsOwn != 0 || after.slotsOwn != 0 {
		t.Errorf("armor after the refusal = %d bin(s), %d slot(s), want none.\n"+
			"A hard claim means a robot is committed; keeping one through a fleet refusal makes the "+
			"books lie — a rank-proof squatter with no wheels.", after.binsOwn, after.slotsOwn)
	}
	wantRes := []string{"bin:pending"}
	if len(after.res) != len(wantRes) || (len(after.res) > 0 && after.res[0] != wantRes[0]) {
		t.Errorf("paper after the refusal = %v, want %v.\n"+
			"DEMOTED, NEVER DELETED. Deleting it while orders.bin_id stays stamped is the pointer "+
			"wedge: dispatchHeldBin confirms by id and never re-acquires, and the confirm requires "+
			"the pending row that was just destroyed.", after.res, wantRes)
	}

	// And it goes, uncontested, on the next attempt — the blip's whole point.
	going, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	order, _ = db.GetOrder(order.ID)
	testutil.MustNoErr(t, going.ConfirmForDispatch(order, binID, srcNode, lineNode),
		"re-confirm the kept paper")
	order, _ = db.GetOrder(order.ID)
	vendorID, err := going.DispatchDirect(order, srcNode, lineNode)
	testutil.MustNoErr(t, err, "re-dispatch after the blip")
	if vendorID == "" {
		t.Error("the re-dispatch produced no vendor order id")
	}
}

// TestFleetRefusal_TheJunctionRowsSurvive pins the fourth book the enumeration
// missed. Today's ReleaseClaimByOrder deletes order_bins; the re-dispatch reads
// those rows to answer which bin a STEP is about (dispatch/bin_for_step.go), and
// bin_id cannot answer it for a multi-bin order.
func TestFleetRefusal_TheJunctionRowsSurvive(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())
	order, binID := armedOrderAwaitingFleet(t, db, d, "demote-junction")
	testutil.MustNoErr(t, orders.InsertOrderBin(db.DB, order.ID, binID, 0, "pickup", "SRC", "DST"),
		"seed the junction row")

	srcNode, lineNode, _ := setupTestData(t, db)
	if _, err := d.DispatchDirect(order, srcNode, lineNode); err == nil {
		t.Fatal("the fleet refused the create; DispatchDirect must report it")
	}

	if n := junctionRows(t, db, order.ID); n != 1 {
		t.Errorf("order_bins rows after the refusal = %d, want 1 kept.\n"+
			"The re-dispatch reads the junction to answer which bin a STEP is about; without it a "+
			"multi-bin order has no per-step answer at all.", n)
	}
}

// TestFleetRefusal_ACompoundLegLeavesItsParentsCorridorAlone is the ownership
// clause, and it is the one that can hurt somebody else.
//
// Lane mouth rows for a compound child belong to its PARENT (laneOwnerFor, §2) —
// one dig chapter, one corridor, held for the whole excavation. A blind
// release-my-lanes on a leg's fleet refusal tears that corridor out from under a
// live dig: the parent's other legs lose their admission and another order walks
// into the lane the dig is working.
//
// THE LEG HOLDS A ROW OF ITS OWN, and without it this test cannot fail. The
// door's DELETE is order-keyed — `WHERE order_id = <the refused order>` — so a
// leg with no mouth row of its own deletes nothing whatever the clause answers,
// and the parent's corridor survives a release the clause never refused. The
// leg's own inbound hold is the row the clause actually protects, and a leg does
// hold one: lane_gate's acquire writes order_id = the LEG's id and carries the
// parent only as the admission asker.
func TestFleetRefusal_ACompoundLegLeavesItsParentsCorridorAlone(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())
	parent, children, laneID, _ := twoLegCompound(t, db, "DEMOLEG")
	leg := children[0]

	// The parent owns the corridor, as it does for every real dig.
	testutil.MustNoErr(t,
		reservations.AcquireLanes(db.DB, parent.ID, reservations.ModeDig, "test-dig", laneID),
		"the parent takes its corridor")
	// And the leg holds its own inbound hold on the lane it is dropping into.
	dropLane := mirrorLane(t, db, "DEMOLEG-DROP", 2)
	testutil.MustNoErr(t,
		reservations.AcquireLanes(db.DB, leg.ID, reservations.ModeInbound, "test-leg", dropLane),
		"the leg takes its own dropoff lane")

	legRow, gerr := db.GetOrder(leg.ID)
	testutil.MustNoErr(t, gerr, "read the leg")
	_, _ = db.DB.Exec(`UPDATE orders SET status=$1 WHERE id=$2`, string(StatusDispatched), leg.ID)
	legRow.Status = StatusDispatched

	d.DemoteAfterFleetRefusal(legRow, protocol.QueueFleetUnavailable, CauseFleetRefusedCreate, QueueParams{})

	mouthRows := func(owner int64) int {
		t.Helper()
		var n int
		testutil.MustNoErr(t, db.DB.QueryRow(
			`SELECT COUNT(*) FROM reservations WHERE order_id=$1 AND resource_kind='mouth'`,
			owner).Scan(&n), "count lane rows")
		return n
	}
	if n := mouthRows(parent.ID); n != 1 {
		t.Errorf("the parent holds %d lane row(s) after its LEG was refused, want 1.\n"+
			"A leg's lane rows belong to its parent. Releasing them on the leg's refusal tears the "+
			"corridor out from under a live dig — the parent's other legs lose their admission and "+
			"anybody may walk in.", n)
	}
	if n := mouthRows(leg.ID); n != 1 {
		t.Errorf("the leg holds %d lane row(s) after its own refusal, want 1 — this is the row the "+
			"ownership clause decides about.\n"+
			"laneOwnerFor(leg) is the PARENT, so the door releases no lane rows for a leg at all: "+
			"its holds are entangled with the chapter its parent is running, and the leg is not "+
			"leaving that chapter over a fleet blip.", n)
	}
}

// TestFleetRefusal_TheDoubleInvocationIsANoOp pins clause 6.
//
// The plain path invokes the rollback TWICE per refusal: DispatchDirect's own
// undo of the CAS it made, and then the caller's park. At SPR fleet failures
// arrive in bursts of 23-41, so a second call that logged an illegal transition
// would print that burst twice over — and worse, a second call that wrote the
// cause again would put two waits in the record where one happened.
func TestFleetRefusal_TheDoubleInvocationIsANoOp(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())
	order, _ := armedOrderAwaitingFleet(t, db, d, "demote-twice")

	srcNode, lineNode, _ := setupTestData(t, db)
	// The inner undo (inside DispatchDirect) fires first...
	if _, err := d.DispatchDirect(order, srcNode, lineNode); err == nil {
		t.Fatal("the fleet refused the create; DispatchDirect must report it")
	}
	// ...and then the caller's park, exactly as the scanner does it.
	order, _ = db.GetOrder(order.ID)
	d.DemoteAfterFleetRefusal(order, protocol.QueueFleetUnavailable, CauseFleetRefusedCreate, QueueParams{})

	// One refusal is one WAIT, so one sourcing row carrying the fleet cause. The
	// order's birth row is `sourcing` too — it was created there — which is why
	// this counts the coded rows rather than every row of that status.
	rows, err := db.ListOrderHistory(order.ID)
	testutil.MustNoErr(t, err, "list order history")
	parks := 0
	for _, h := range rows {
		if h.Status == StatusSourcing && h.Code == string(protocol.QueueFleetUnavailable) {
			parks++
		}
	}
	if parks != 1 {
		var got []string
		for _, h := range rows {
			got = append(got, string(h.Status)+"/"+h.Code)
		}
		t.Errorf("the order has %d fleet-refusal waits in its history after ONE refusal, want 1; "+
			"history is %v.\nOne refusal is one wait. A second row makes a burst of 23-41 read as "+
			"twice that.", parks, got)
	}
}
