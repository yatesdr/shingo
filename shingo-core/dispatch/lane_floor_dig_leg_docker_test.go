//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// THE FLOOR LEAVES A DIG LEG THAT HAS NOT LIFTED ALONE.
//
// A dig leg dispatches with its plan already parked at the lane wait it will
// stand at once it has the blocker up. Until the lift, the robot is driving to
// the blocker, not standing in the lane, and the lift is what releases it: Core
// chooses where the blocker goes and appends the tail. The floor counted the leg
// as waiting from dispatch, so a floor tick between dispatch and the lift bound
// its destination, appended its tail and recorded a robot "standing still" that
// was driving.
//
// Wanted: before the lift the floor binds nothing, appends nothing and records
// nothing. Once the blocker is up and the lift's own release has gone missing,
// the floor frees it as before.
func TestFloor_LeavesADigLegThatHasNotLiftedAlone(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)
	_, dug, _, park, dugSlots, _, bp := setupDwellGroup(t, db, "FLDIG", 2, true)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "FLDIG-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "FLDIG-TGT")
	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "fldig"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "FLDIG-LINE").Name
		o.Status = protocol.StatusPending
	})
	leg := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)[0]
	if leg.VendorOrderID == "" || leg.DeliveryNode != "" || !IsGateStaged(leg) || leg.BinID == nil {
		t.Fatalf("fixture: the first leg is not a dispatched dig leg awaiting its tail "+
			"(vendor %q, destination %q, gate-staged %v)", leg.VendorOrderID, leg.DeliveryNode, IsGateStaged(leg))
	}

	// ── THE ROBOT IS STILL DRIVING TO THE BLOCKER ────────────────────────────
	freed := d.SweepLaneWaiters()
	after, err := db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "reload the leg")
	if freed != 0 || after.DeliveryNode != "" || after.WaitIndex != leg.WaitIndex ||
		appendsTo(backend, leg.VendorOrderID) != 0 {
		t.Fatalf("the floor released a dig leg that has not lifted: freed %d, destination %q, wait index "+
			"%d -> %d, %d tail append(s). Its blocker is still in its slot, so the robot is driving to it, "+
			"and the lift is what releases it", freed, after.DeliveryNode, leg.WaitIndex, after.WaitIndex,
			appendsTo(backend, leg.VendorOrderID))
	}
	if recs := floorReleaseRecords(t, db); len(recs) != 0 {
		t.Fatalf("the floor recorded %d release(s) for a robot that was driving: %q", len(recs), recs)
	}

	// ── THE LIFT LANDS AND ITS RELEASE GOES MISSING ──────────────────────────
	// Only the durable half of the lift: the blocker leaves its slot. No event.
	transit, err := db.GetNodeByName(domain.TransitNodeName)
	testutil.MustNoErr(t, err, "find the transit node")
	testutil.MustNoErr(t, db.MoveBinToTransit(*leg.BinID, transit.ID), "lift the blocker")

	freed = d.SweepLaneWaiters()
	after, err = db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "reload the leg")
	// The pass frees the leg and then the dig's next leg, which the first leg's
	// drive-out lets in; one record each.
	if freed == 0 || after.DeliveryNode != park.Name || appendsTo(backend, leg.VendorOrderID) != 1 {
		t.Fatalf("a lifted dig leg whose release went missing was not freed by the floor: freed %d, "+
			"destination %q (want %s), %d tail append(s)", freed, after.DeliveryNode, park.Name,
			appendsTo(backend, leg.VendorOrderID))
	}
	if recs := floorReleaseRecords(t, db); len(recs) != freed {
		t.Errorf("floor records = %d, want one for each of the %d order(s) it freed", len(recs), freed)
	}
}

// A LANE EVENT LEAVES A DIG LEG THAT HAS NOT LIFTED ALONE, TOO.
//
// The periodic lane check already waits for the lift. The event path must ask
// the same question, or the two find different sets of waiting orders: here a
// second dig's first leg is driving to its blocker when a sibling dig's leg in
// the same group is released, and the group fan-out that release wakes runs the
// evaluator over the unlifted leg's lane. It must bind nothing and append
// nothing until that leg's own lift.
func TestLaneEvent_LeavesADigLegThatHasNotLiftedAlone(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)
	grp, dug, sib, _, dugSlots, sibSlots, bp := setupDwellGroup(t, db, "EVDIG", 2, true)
	// A second parking spot, so each dig has somewhere to put its blocker.
	testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: "EVDIG-PARK2", ParentID: &grp.ID, Enabled: true}),
		"create the second parking spot")

	digIn := func(lane *nodes.Node, slots []*nodes.Node, tag string) *orders.Order {
		createTestBinAtNode(t, db, bp.Code, slots[0].ID, "EVDIG-BLK-"+tag)
		createTestBinAtNode(t, db, bp.Code, slots[1].ID, "EVDIG-TGT-"+tag)
		demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = "evdig-" + tag
			o.OrderType = OrderTypeRetrieve
			o.PayloadCode = bp.Code
			o.DeliveryNode = lineNode(t, db, "EVDIG-LINE-"+tag).Name
			o.Status = protocol.StatusPending
		})
		leg := planDigFor(t, db, d, bp, lane, slots[1], demand)[0]
		if leg.VendorOrderID == "" || leg.DeliveryNode != "" || !IsGateStaged(leg) || leg.BinID == nil {
			t.Fatalf("fixture: dig %s's first leg is not a dispatched dig leg awaiting its tail "+
				"(vendor %q, destination %q, gate-staged %v)", tag, leg.VendorOrderID, leg.DeliveryNode,
				IsGateStaged(leg))
		}
		return leg
	}
	lifted := digIn(dug, dugSlots, "A")
	driving := digIn(sib, sibSlots, "B")

	// ── DIG A LIFTS AND IS RELEASED BY ITS LIFT ──────────────────────────────
	transit, err := db.GetNodeByName(domain.TransitNodeName)
	testutil.MustNoErr(t, err, "find the transit node")
	liftBin(t, db, d, lifted, dugSlots[0], transit)
	d.EvaluateLaneReleases(dug.ID)
	afterA, err := db.GetOrder(lifted.ID)
	testutil.MustNoErr(t, err, "reload dig A's leg")
	if afterA.DeliveryNode == "" || appendsTo(backend, lifted.VendorOrderID) != 1 {
		t.Fatalf("fixture: dig A's lifted leg was not released (destination %q, %d tail append(s))",
			afterA.DeliveryNode, appendsTo(backend, lifted.VendorOrderID))
	}

	// ── THE RELEASE WAKES THE GROUP; DIG B HAS NOT LIFTED ────────────────────
	d.EvaluateDwellersSharingGroupWith(dugSlots[0].ID)
	afterB, err := db.GetOrder(driving.ID)
	testutil.MustNoErr(t, err, "reload dig B's leg")
	if afterB.DeliveryNode != "" || afterB.WaitIndex != driving.WaitIndex ||
		appendsTo(backend, driving.VendorOrderID) != 0 {
		t.Fatalf("a lane event released a dig leg that has not lifted: destination %q, wait index %d -> %d, "+
			"%d tail append(s). Its blocker is still in %s, so the robot is driving to it, and the lift is "+
			"what releases it", afterB.DeliveryNode, driving.WaitIndex, afterB.WaitIndex,
			appendsTo(backend, driving.VendorOrderID), sibSlots[0].Name)
	}

	// ── B LIFTS: ITS OWN EVENT RELEASES IT ───────────────────────────────────
	liftBin(t, db, d, driving, sibSlots[0], transit)
	d.EvaluateLaneReleases(sib.ID)
	afterB, err = db.GetOrder(driving.ID)
	testutil.MustNoErr(t, err, "reload dig B's leg")
	if afterB.DeliveryNode == "" || appendsTo(backend, driving.VendorOrderID) != 1 {
		t.Fatalf("dig B's leg was not released at its lift: destination %q, %d tail append(s)",
			afterB.DeliveryNode, appendsTo(backend, driving.VendorOrderID))
	}
}
