//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestSlotClaim_NoABBADeadlock is the acquire-level port of the canonical-sort pin.
// The hard-claim slot loop and its node-ID sort are gone; the ABBA class dissolves
// at the soft-acquire layer instead. Two orders whose FIXED-concrete storage
// drop-offs are {S1, S2} in opposite step order can no longer cross-hold HARD
// claims: a slot is hard-claimed (ConfirmSlotClaim) only AFTER the whole slot set
// is reserved, so a blocked order reserves what it can (revocable) and backs off
// WITHOUT ever hard-claiming the far slot. No order holds a hard claim while
// blocked — the unbounded-deadlock ingredient the canonical sort prevented is
// simply absent, without any ordering.
//
// It also pins slots-before-bins: the blocked order returns at the slot reserve leg
// holding ZERO bin reservations — bins are never touched before slots are secured.
func TestSlotClaim_NoABBADeadlock(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Two fixed-concrete storage slots under one NGRP.
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP type")
	grp := &nodes.Node{Name: "ABBA-NGRP", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create NGRP")
	s1 := &nodes.Node{Name: "ABBA-S1", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(s1), "create S1")
	s2 := &nodes.Node{Name: "ABBA-S2", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(s2), "create S2")

	// Order A already holds a slot RESERVATION on S1 (soft — the reservation-native
	// stand-in for "A got there first").
	orderA := &orders.Order{EdgeUUID: "abba-A", StationID: "line-1", OrderType: OrderTypeComplex, Status: StatusSourcing, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(orderA), "create order A")
	testutil.MustNoErr(t, db.ReserveSlot(s1.ID, orderA.ID), "A reserves S1")

	// Order B drops at S2 THEN S1 — opposite step order, fixed-concrete (no group).
	orderB := &orders.Order{
		EdgeUUID: "abba-B", StationID: "line-1", OrderType: OrderTypeComplex, Status: StatusQueued, Quantity: 1,
		SourceNode: lineNode.Name, DeliveryNode: s1.Name, PayloadCode: bp.Code,
		StepsJSON: `[{"action":"pickup","node":"` + lineNode.Name + `"},` +
			`{"action":"dropoff","node":"` + s2.Name + `"},` +
			`{"action":"dropoff","node":"` + s1.Name + `"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(orderB), "create order B")
	orderB, _ = db.GetOrder(orderB.ID)

	// B attempts dispatch: it must requeue on the contended slot, not dispatch.
	if err := d.DispatchPreparedComplex(orderB); err == nil {
		t.Fatal("order B dispatched; expected a slot-reserve requeue (non-nil error)")
	}

	// ABBA dissolution: B, blocked on S1, holds NO HARD claim on the far slot S2 — it
	// backs off holding only a revocable reservation. There is no hard claim to order.
	if s2n, _ := db.GetNode(s2.ID); s2n.ClaimedBy != nil {
		t.Fatalf("S2 claimed_by=%d — the loser hard-grabbed the far slot; the soft-acquire layer must leave it a revocable reservation only (ABBA)", *s2n.ClaimedBy)
	}
	if s1n, _ := db.GetNode(s1.ID); s1n.ClaimedBy != nil {
		t.Fatalf("S1 claimed_by=%d — no order should hold a hard slot claim while blocked", *s1n.ClaimedBy)
	}

	// A's S1 reservation is intact (B did not steal it).
	aHeld, _ := db.ListReservationsByOrder(orderA.ID)
	if len(aHeld) != 1 || aHeld[0].Kind != reservations.KindSlot || aHeld[0].NodeID != s1.ID {
		t.Fatalf("A's holds = %+v, want exactly a slot reservation on S1=%d", aHeld, s1.ID)
	}

	// Slots-before-bins: B returned at the slot reserve leg, so it holds ZERO BIN
	// reservations (it may hold a revocable slot reservation on S2 — that's the point).
	bHeld, _ := db.ListReservationsByOrder(orderB.ID)
	for _, r := range bHeld {
		if r.Kind == reservations.KindBin {
			t.Fatal("order B holds a bin reservation — bins must not be touched before slots are secured (slots-before-bins)")
		}
	}

	// B is requeued (not terminal), free to retry once A releases S1.
	gotB, _ := db.GetOrder(orderB.ID)
	if gotB.Status != StatusQueued && gotB.Status != StatusSourcing {
		t.Errorf("order B status=%q, want queued/sourcing (requeued to retry)", gotB.Status)
	}
}
