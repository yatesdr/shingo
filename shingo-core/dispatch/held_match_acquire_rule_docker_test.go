//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// TestReserveReconcile_AHeldBinIsKeptByTheRuleThatAcquiredIt: a pickup acquires
// a bin through findAvailableForNeed, which takes stock of the part for a
// payload pickup and any resident bin, an empty included, for the removal of
// the order's own line position. Re-reserving the same plan must recognise
// those holds as its own. Matched only by (node, empty-status), an empty bin
// held for a non-empty removal read as someone else's: "bins present but none
// available", every pass, for a bin the order itself held. A pass that prepares
// twice (the re-make on the pass that reaches the hand) parked on it for ever:
// the keep-staged cell's level-keeper and REQUEST swaps after a cancelled
// changeover.
func TestReserveReconcile_AHeldBinIsKeptByTheRuleThatAcquiredIt(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, lineNode, bp := testdb.SetupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	spot := &nodes.Node{Name: "HM-SPOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(spot), "spot node")
	carrier := testdb.CreateBinAtNode(t, db, bp.Code, spot.ID, "HM-CARRIER")
	lineBin := testdb.CreateBinAtNode(t, db, "", lineNode.ID, "HM-LINE")

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: lineNode.Name}, // removal: any resident bin, this empty too
		{Action: protocol.ActionDropoff, Node: spot.Name + "-OUT"},
		{Action: protocol.ActionPickup, Node: spot.Name, PayloadCode: bp.Code}, // payload pickup: stock of the part
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	order := mkComplexOrder(t, db, "held-match-1", spot.Name, lineNode.Name, lineNode.Name, bp.Code, steps)
	plan := BuildComplexPlan(steps)

	_, first, err := d.allocator.reserveComplexPlan(order, plan, d.db)
	testutil.MustNoErr(t, err, "first reserve")
	if first != reserveComplete {
		t.Fatalf("first reserve = %v, want reserveComplete (the empty carrier and the line bin are both takeable)", first)
	}
	_, again, err := d.allocator.reserveComplexPlan(order, plan, d.db)
	testutil.MustNoErr(t, err, "second reserve")
	if again != reserveComplete {
		t.Fatalf("second reserve of the same plan = %v, want reserveComplete: the order's own holds read as taken", again)
	}
	res, err := db.ListReservationsByOrder(order.ID)
	testutil.MustNoErr(t, err, "list reservations")
	if !reservesExactly(res, carrier.ID, lineBin.ID) {
		t.Fatalf("held = %+v, want exactly the carrier %d and the line bin %d", res, carrier.ID, lineBin.ID)
	}
}

// TestReserveReconcile_AStockPickupKeepsNoHeldEmpty: an empty the order holds at
// a payload pickup's node (held before the bind took stock of the part alone,
// or by a hand) is not that pickup's: the bind would never take it for it
// (rule 5), so the reconcile does not keep it either. The pickup misses, the
// order holds, and the stray hold is released. A removal and an empty leg still
// keep a hold of the other empty-status: whatever stands at the line, and the
// partial an empty leg finishes.
func TestReserveReconcile_AStockPickupKeepsNoHeldEmpty(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, lineNode, bp := testdb.SetupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	spot := &nodes.Node{Name: "HM2-SPOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(spot), "spot node")
	empty := testdb.CreateBinAtNode(t, db, "", spot.ID, "HM2-EMPTY")
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: spot.Name, PayloadCode: bp.Code},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}
	order := mkComplexOrder(t, db, "held-match-2", spot.Name, lineNode.Name, lineNode.Name, bp.Code, steps)
	testdb.ReserveBin(t, db, order.ID, empty.ID)

	_, out, err := d.allocator.reserveComplexPlan(order, BuildComplexPlan(steps), d.db)
	testutil.MustNoErr(t, err, "reserve")
	if out == reserveComplete {
		t.Fatalf("reserve = reserveComplete: the payload pickup kept the empty it holds as stock of %s", bp.Code)
	}
	res, err := db.ListReservationsByOrder(order.ID)
	testutil.MustNoErr(t, err, "list reservations")
	if len(res) != 0 {
		t.Errorf("held = %+v, want none: the stray hold on the empty %d is released", res, empty.ID)
	}
}

// TestResolveHeldReservations_AgreesWithTheRowsOneAtATime characterises the
// reconcile's read of an order's holds (resolveHeldReservations): for a slot row
// (pending, and confirmed on a lane's child), a bin row on a node (full, and
// empty on a lane's child), a bin row whose bin stands nowhere, a bin row whose
// bin does not exist and a slot row whose node does not exist, every field it
// answers is what the doors answer one row at a time: the bin by id (GetBin),
// then the bin's node or the slot's by id (GetNode). It is asked twice: of rows
// as written here, each carrying what the listing reads with it (a slot row's
// node name), and of an order's rows as the store lists them
// (ListReservationsByOrder), which the reconcile is handed. The want column
// names each row's node, so a fixture that stops building what it says fails
// here rather than agreeing vacuously.
func TestResolveHeldReservations_AgreesWithTheRowsOneAtATime(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, lineNode, bp := testdb.SetupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	mk := func(name string, parent *int64) *nodes.Node {
		t.Helper()
		n := &nodes.Node{Name: name, Enabled: true, ParentID: parent}
		testutil.MustNoErr(t, db.CreateNode(n), "node "+name)
		return n
	}
	lane := mk("RH-LANE", nil)
	slotA, slotB := mk("RH-SLOT-A", nil), mk("RH-SLOT-B", &lane.ID)
	full, empty := mk("RH-FULL", nil), mk("RH-EMPTY", &lane.ID)
	fullBin := testdb.CreateBinAtNode(t, db, bp.Code, full.ID, "RH-FULL-BIN")
	emptyBin := testdb.CreateBinAtNode(t, db, "", empty.ID, "RH-EMPTY-BIN")
	nowhere := testdb.CreateBinAtNode(t, db, bp.Code, full.ID, "RH-NOWHERE-BIN")
	order := mkComplexOrder(t, db, "rh-listed", full.Name, lineNode.Name, slotA.Name, bp.Code, []resolvedStep{
		{Action: protocol.ActionPickup, Node: full.Name},
		{Action: protocol.ActionDropoff, Node: slotA.Name},
	})
	testutil.MustNoErr(t, db.ReserveSlot(slotA.ID, order.ID), "reserve slot A")
	testutil.MustNoErr(t, db.ReserveSlot(slotB.ID, order.ID), "reserve slot B")
	testutil.MustNoErr(t, db.ConfirmSlotReservation(slotB.ID, order.ID), "confirm slot B")
	for _, b := range []int64{fullBin.ID, emptyBin.ID, nowhere.ID} {
		testdb.ReserveBin(t, db, order.ID, b)
	}
	_, err := db.Exec(`UPDATE bins SET node_id = NULL WHERE id = $1`, nowhere.ID)
	testutil.MustNoErr(t, err, "stand a bin nowhere")
	nodeName := func(id int64) string {
		if n, err := db.GetNode(id); err == nil && n != nil {
			return n.Name
		}
		return ""
	}
	check := func(label string, rows []reservations.Reservation, wantNode []string) {
		t.Helper()
		got := d.allocator.resolveHeldReservations(rows)
		if len(got) != len(rows) || len(rows) != len(wantNode) {
			t.Fatalf("%s: %d holds for %d rows, %d named", label, len(got), len(rows), len(wantNode))
		}
		for i, r := range rows {
			want := heldReservation{kind: r.Kind, confirmed: r.State == reservations.StateConfirmed}
			if r.Kind == reservations.KindSlot {
				want.nodeID, want.nodeName = r.NodeID, nodeName(r.NodeID)
			} else {
				want.binID = r.BinID
				if b, err := db.GetBin(r.BinID); err == nil && b != nil {
					want.empty = b.PayloadCode == ""
					if b.NodeID != nil {
						want.nodeID, want.nodeName = *b.NodeID, nodeName(*b.NodeID)
					}
				}
			}
			if *got[i] != want {
				t.Errorf("%s, row %d (%+v): resolveHeldReservations = %+v, one row at a time %+v", label, i, r, *got[i], want)
			}
			if got[i].nodeName != wantNode[i] {
				t.Errorf("%s, row %d (%+v): node %q, want %q", label, i, r, got[i].nodeName, wantNode[i])
			}
		}
	}

	const gone = int64(1) << 40 // no bin and no node has this id
	check("rows as written", []reservations.Reservation{
		{Kind: reservations.KindSlot, NodeID: slotA.ID, State: reservations.StatePending, NodeName: slotA.Name},
		{Kind: reservations.KindSlot, NodeID: slotB.ID, State: reservations.StateConfirmed, NodeName: slotB.Name},
		{Kind: reservations.KindBin, BinID: fullBin.ID, State: reservations.StatePending},
		{Kind: reservations.KindBin, BinID: emptyBin.ID, State: reservations.StateConfirmed},
		{Kind: reservations.KindBin, BinID: nowhere.ID, State: reservations.StatePending},
		{Kind: reservations.KindBin, BinID: gone, State: reservations.StatePending},
		{Kind: reservations.KindSlot, NodeID: gone, State: reservations.StatePending},
	}, []string{slotA.Name, slotB.Name, full.Name, empty.Name, "", "", ""})

	listed, err := db.ListReservationsByOrder(order.ID)
	testutil.MustNoErr(t, err, "list the order's reservations")
	check("rows as listed", listed, []string{slotA.Name, slotB.Name, full.Name, empty.Name, ""})
}
