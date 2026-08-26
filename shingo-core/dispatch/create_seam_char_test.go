//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// create_seam_char_test.go — the BEFORE photograph.
//
// Four dispatch arms reach the fleet, and each one decides for itself whether to
// record that its robot is about to be inside a lane. This file pins what each
// arm does TODAY, ahead of extracting the seam they should share. Its whole
// value is that it is written before the change and must survive it unmodified:
// a characterization test that has to be edited to stay green is a behaviour
// change wearing a refactor's clothes.
//
// The coverage is deliberately weighted toward COMPLEX. Both plants run complex
// orders for most of their lane traffic, and complex is the arm the extraction
// changes.
//
// WHAT "RECORD ITS PRESENCE" MEANS HERE. Hold B — a row in `reservations` with
// resource_kind='occupancy', keyed (order, lane). Admission refuses a lane whose
// occupants include anyone but the asker, so the row is how one order's presence
// becomes another order's refusal. An arm that reads the answer and never writes
// into it is invisible to everyone else, which is the defect this file is the
// prelude to.

// seamLane builds an ungated lane with two slots and returns the lane and its
// mouth. Ungated on purpose: the gated arm is characterized separately, and a
// mark would change which seam the order takes.
func seamLane(t *testing.T, db *store.DB, prefix string) (laneID int64, mouth *nodes.Node) {
	t.Helper()
	_, laneID, mouth = gatedLane(t, db, prefix, "")
	return laneID, mouth
}

// TestCharSeam_PlainStore_TakesOccupancyBeforeTheCreate is the reference arm —
// the shape the other three are measured against.
//
// The take is BEFORE the handover, and that ordering is the point rather than an
// accident: a robot committed to the fleet with its presence unrecorded leaves a
// lane that reads empty to the next order, which is the collision from the other
// side. dispatcher.go says so where it does it.
func TestCharSeam_PlainStore_TakesOccupancyBeforeTheCreate(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	srcNode, _, bp := setupTestData(t, db)
	laneID, mouth := seamLane(t, db, "CSEAM-PLAIN")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	bin := testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "CSEAM-PLAIN-BIN")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "cseam-plain"
		o.OrderType = OrderTypeMove
		o.PayloadCode = bp.Code
		o.SourceNode = srcNode.Name
		o.DeliveryNode = mouth.Name
		o.Status = StatusSourcing
	})
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, bin.ID), "stamp the bin")
	order, _ = db.GetOrder(order.ID)

	_, dErr := d.DispatchDirect(order, srcNode, mouth)
	testutil.MustNoErr(t, dErr, "the plain dispatch must go")

	got := occupants(t, db, laneID)
	if len(got) != 1 || got[0] != order.ID {
		t.Fatalf("occupants of the destination lane = %v, want exactly [%d].\n"+
			"The plain arm records its presence at the create seam; admission refuses a lane on "+
			"any occupant but the asker, so this row is how this robot becomes everyone else's "+
			"refusal.", got, order.ID)
	}
}

// TestCharSeam_PlainStore_ReleasesOccupancyWhenTheCreateFails pins the other
// half of the plain arm, and it is the half an extraction is most likely to drop.
//
// Taking before the handover is only safe because every failure that leaves no
// robot in the lane gives the row back. A row left behind by a create that never
// happened wedges the lane forever, with nothing alive to release it.
func TestCharSeam_PlainStore_ReleasesOccupancyWhenTheCreateFails(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	srcNode, _, bp := setupTestData(t, db)
	laneID, mouth := seamLane(t, db, "CSEAM-FAIL")
	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())

	bin := testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "CSEAM-FAIL-BIN")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "cseam-fail"
		o.OrderType = OrderTypeMove
		o.PayloadCode = bp.Code
		o.SourceNode = srcNode.Name
		o.DeliveryNode = mouth.Name
		o.Status = StatusSourcing
	})
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, bin.ID), "stamp the bin")
	order, _ = db.GetOrder(order.ID)

	if _, err := d.DispatchDirect(order, srcNode, mouth); err == nil {
		t.Fatal("the fleet refused the create; the dispatch must report it")
	}
	if got := occupants(t, db, laneID); len(got) != 0 {
		t.Fatalf("occupants after a REFUSED create = %v, want none.\n"+
			"No robot went anywhere, so a surviving row holds the lane against every other order "+
			"with nothing alive to clear it.", got)
	}
}

// TestCharSeam_CompoundLeg_TakesOccupancyAtTheAdvance pins the dig arm.
//
// It takes its row in AdvanceCompoundOrder, before the status CAS and therefore
// well before the create — earlier than the plain arm, and against a lane its
// parent already holds as a dig. Recorded rather than judged: the audit calls the
// early take an extraction candidate, and this test is what will tell anyone who
// moves it what they changed.
func TestCharSeam_CompoundLeg_TakesOccupancyAtTheAdvance(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	parent, children, lane, _ := twoLegCompound(t, db, "CSEAMDIG")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "advance onto leg one")

	got := occupants(t, db, lane)
	if len(got) != 1 || got[0] != children[0].ID {
		t.Fatalf("occupants after advancing onto leg one = %v, want exactly [%d] (the LEG, not the "+
			"parent — presence is per robot, and the parent is not one)", got, children[0].ID)
	}
}

// TestCharSeam_GatedCreate_TakesNoOccupancyUntilTheTail pins the arm whose
// answer is deliberately "not yet".
//
// A gated create ends at the wait point OUTSIDE the corridor. A row taken there
// would say a robot is inside a lane it is specifically parked out of, walling
// the lane for the whole dwell — so the gated arm defers its take to the tail
// append, which is the moment entry actually happens. This is the one arm whose
// missing row at the create is correct, and the extraction must not "fix" it.
func TestCharSeam_GatedCreate_TakesNoOccupancyUntilTheTail(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	srcNode, _, bp := setupTestData(t, db)
	_, laneID, mouth := gatedLane(t, db, "CSEAM-GATED", "CSEAM-GATED-WAIT")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// A second order physically inside, so the gate holds the dweller out and the
	// tail is withheld. Without it the valve appends immediately and the dwell —
	// the state under test — never exists.
	blocker := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "cseam-gated-blocker"
		o.Status = StatusDispatched
	})
	testutil.MustNoErr(t, d.TakeLaneOccupancy(blocker.ID, mouth), "the blocker occupies the lane")

	bin := testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "CSEAM-GATED-BIN")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "cseam-gated"
		o.OrderType = OrderTypeMove
		o.PayloadCode = bp.Code
		o.SourceNode = srcNode.Name
		o.DeliveryNode = mouth.Name
		o.Status = StatusSourcing
	})
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, bin.ID), "stamp the bin")
	order, _ = db.GetOrder(order.ID)
	_, gErr := d.DispatchDirect(order, srcNode, mouth)
	testutil.MustNoErr(t, gErr, "the gated dispatch must create, unsealed")

	for _, occ := range occupants(t, db, laneID) {
		if occ == order.ID {
			t.Fatalf("the gate-staged order %d holds an occupancy row while it is dwelling OUTSIDE "+
				"the corridor. That row walls the lane for the length of the dwell, which is the "+
				"opposite of what staging is for.", order.ID)
		}
	}
}

// TestCharSeam_ComplexUngated_TakesItsOccupancyRow is the positive twin of the
// characterization test that recorded the defect, and it replaces it — as that
// test's own failure message asked the fixing commit to do.
//
// What it pins: a complex order that dispatches into a lane appears in the
// occupancy ledger, so the gate can protect everyone else from it. Before this,
// it read every other order's presence and wrote none of its own.
func TestCharSeam_ComplexUngated_TakesItsOccupancyRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	srcNode, _, bp := setupTestData(t, db)
	laneID, mouth := seamLane(t, db, "CSEAM-CPLX")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "CSEAM-CPLX-BIN")
	order := &orders.Order{
		EdgeUUID:     "cseam-complex",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     1,
		PayloadCode:  bp.Code,
		SourceNode:   srcNode.Name,
		DeliveryNode: mouth.Name,
		ProcessNode:  srcNode.Name,
		StepsJSON: `[{"action":"pickup","node":"` + srcNode.Name + `"},` +
			`{"action":"dropoff","node":"` + mouth.Name + `"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create the complex order")
	order, _ = db.GetOrder(order.ID)

	testutil.MustNoErr(t, d.DispatchPreparedComplex(order), "the complex dispatch must go")

	sent, _ := db.GetOrder(order.ID)
	if sent.VendorOrderID == "" {
		t.Fatalf("the complex order never reached the fleet (status %q) — this test says nothing "+
			"about occupancy unless a robot was actually committed", sent.Status)
	}
	got := occupants(t, db, laneID)
	if len(got) != 1 || got[0] != order.ID {
		t.Fatalf("occupants of the lane a complex robot was just sent into = %v, want [%d]",
			got, order.ID)
	}
}

// TestCharSeam_ComplexIsRefusedByAnotherOrdersOccupancy pins the half that DOES
// work, and it is the half that makes the missing write worth fixing rather than
// worth deleting.
//
// Complex asks the question properly: a lane somebody else occupies refuses it,
// and it parks instead of driving in. So the machinery is one row away from
// symmetric — the reader is wired, only the writer is absent.
func TestCharSeam_ComplexIsRefusedByAnotherOrdersOccupancy(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	srcNode, _, bp := setupTestData(t, db)
	_, mouth := seamLane(t, db, "CSEAM-CREAD")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	sitting := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "cseam-cread-sitting"
		o.Status = StatusDispatched
	})
	testutil.MustNoErr(t, d.TakeLaneOccupancy(sitting.ID, mouth), "somebody else is inside")

	testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "CSEAM-CREAD-BIN")
	order := &orders.Order{
		EdgeUUID:     "cseam-cread",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     1,
		PayloadCode:  bp.Code,
		SourceNode:   srcNode.Name,
		DeliveryNode: mouth.Name,
		ProcessNode:  srcNode.Name,
		StepsJSON: `[{"action":"pickup","node":"` + srcNode.Name + `"},` +
			`{"action":"dropoff","node":"` + mouth.Name + `"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create the complex order")
	order, _ = db.GetOrder(order.ID)

	_ = d.DispatchPreparedComplex(order)

	got, _ := db.GetOrder(order.ID)
	if got.VendorOrderID != "" {
		t.Fatalf("the complex order dispatched as %q into a lane order %d is inside. The READ half "+
			"of Hold B is what stops this, and it is the half that already works.",
			got.VendorOrderID, sitting.ID)
	}
	if protocol.IsTerminal(got.Status) {
		t.Errorf("the complex order is %q — a busy lane is congestion and the order must wait for it, "+
			"not die of it", got.Status)
	}
}
