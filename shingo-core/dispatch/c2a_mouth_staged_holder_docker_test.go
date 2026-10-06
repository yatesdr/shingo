//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// c2a_mouth_staged_holder_docker_test.go — the mouth clause's grant is "released
// into it, tail appended, not yet landed" (RULINGS-C1-pair-and-C2a-go §Amendment,
// ruling 1). A claim whose holder stands STAGED at a mark has not been released
// into the lane, and the clause counted it anyway.
//
// Found on the houseserver after the wedge fix (runs/hs/S2-S3-after-5d0a36e2-HELD-MOUTH.md):
// S.2 order 214, an in-lane move whose final drop is LS_C1's mouth, held that
// slot from dispatch and refused its own dig and 414's (1668 refusals); S.3
// order 296, staged at its mark under lane-deeper-pending, held Lane_05's mouth
// while waiting on 289, whose dig its claim refused (a cycle); S.3 swap 218 held
// Lane_08's mouth from its hand and never entered it (227: 1031 refusals).
// Completed fell 719 -> 544 (S.2) and 621 -> 477 (S.3).
//
// Both shapes: a three-slot dug lane [S1 mouth claimed and empty; S2 a bin; S3
// a bin or empty behind], the claim's holder staged at a mark.

// TestC2aPin_ARequesterWhoseOwnDropIsTheMouthGetsItsDig: 214's shape. The
// requester's pickup is S3, walled by S2; its own final drop is S1, claimed.
func TestC2aPin_ARequesterWhoseOwnDropIsTheMouthGetsItsDig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, _, _, dugSlots, _, bp := setupDwellGroup(t, db, "C2AO", 3, false)
	mouth, wall, pick := dugSlots[0], dugSlots[1], dugSlots[2]
	createTestBinAtNode(t, db, bp.Code, wall.ID, "C2AO-WALL")
	target := createTestBinAtNode(t, db, bp.Code, pick.ID, "C2AO-TARGET")

	requester := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "c2ao-move"
		o.StationID = "line-1"
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.PayloadCode = bp.Code
		o.Status = protocol.StatusStaged
		o.SourceNode = pick.Name
		o.DeliveryNode = mouth.Name
	})
	testutil.MustNoErr(t, db.UpdateOrderVendor(requester.ID, "C2AO-V1", "WAITING", "AMR-1"), "vendor order")
	testdb.ReserveBin(t, db, requester.ID, target.ID)
	claimAndConfirm(t, d, requester, mouth.ID)

	res := d.proposeLaneClearDig(dug, pick, requester)
	if res.outcome != laneClearStarted {
		t.Fatalf("the requester's dig of %s to reach its pickup %s answered %v (%v): its own claim on the "+
			"mouth %s, held while it stands at the mark, is not a grant", dug.Name, pick.Name, res.outcome,
			res.err, mouth.Name)
	}
}

// TestC2aPin_AnOrderStagedAtItsMarkDoesNotHoldTheMouthAgainstADig: 296's and
// 218's shape. Another order holds the mouth's claim while it stands at a mark;
// the requester's slot is S3, behind the wall.
func TestC2aPin_AnOrderStagedAtItsMarkDoesNotHoldTheMouthAgainstADig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, _, _, dugSlots, _, bp := setupDwellGroup(t, db, "C2AH", 3, false)
	mouth, wall, behind := dugSlots[0], dugSlots[1], dugSlots[2]
	createTestBinAtNode(t, db, bp.Code, wall.ID, "C2AH-WALL")

	waiter := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "c2ah-waiter"
		o.StationID = "line-1"
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.PayloadCode = bp.Code
		o.Status = protocol.StatusStaged
		o.DeliveryNode = mouth.Name
	})
	testutil.MustNoErr(t, db.UpdateOrderVendor(waiter.ID, "C2AH-V1", "WAITING", "AMR-1"), "vendor order")
	claimAndConfirm(t, d, waiter, mouth.ID)

	requester := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "c2ah-store"
		o.StationID = "line-1"
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.PayloadCode = bp.Code
		o.Status = protocol.StatusStaged
		o.DeliveryNode = behind.Name
	})
	testutil.MustNoErr(t, db.UpdateOrderVendor(requester.ID, "C2AH-V2", "WAITING", "AMR-2"), "vendor order")
	claimAndConfirm(t, d, requester, behind.ID)

	res := d.proposeLaneClearDig(dug, behind, requester)
	if res.outcome != laneClearStarted {
		t.Fatalf("the dig of %s to open %s answered %v (%v): order %d holds the mouth %s while it stands "+
			"at its mark, and has not been released into the lane", dug.Name, behind.Name, res.outcome,
			res.err, waiter.ID, mouth.Name)
	}
}

// claimAndConfirm takes and confirms a slot claim the way the gate's release does.
func claimAndConfirm(t *testing.T, d *Dispatcher, o *orders.Order, nodeID int64) {
	t.Helper()
	n, err := d.db.GetNode(nodeID)
	testutil.MustNoErr(t, err, "read the slot")
	testutil.MustNoErr(t, claimStoreSlot(d.db, o, n), "claim "+n.Name)
	n, err = d.db.GetNode(nodeID)
	testutil.MustNoErr(t, err, "reload the slot")
	testutil.MustNoErr(t, d.confirmDropoffSlot(o, n), "confirm "+n.Name)
}
