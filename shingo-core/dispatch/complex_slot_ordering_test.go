//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestSlotClaim_NoABBADeadlock pins the canonical slot-claim ordering (D18-Q5, commit 6).
// Two orders whose FIXED-concrete storage drop-offs are {S1, S2} in OPPOSITE step order must
// not cross-hold: the slot-claim loop claims in node-ID order, not step order, so the loser
// fails its FIRST contended slot and backs off holding NOTHING — it never grabs the far slot
// the winner still needs. Without the sort, order B (step order S2→S1) would grab the free S2
// first and then wedge on S1, holding S2 while A waits on it — the ABBA deadlock, now
// unbounded since commit 5 stopped abandoning pre-dispatch orders on a timer.
//
// It also pins SLOTS-BEFORE-BINS: the loser returns at the slot loop with ZERO bin
// reservations, proving bins are never touched before slots are secured — the ordering that
// keeps a slot↔bin cross-type cycle from forming.
func TestSlotClaim_NoABBADeadlock(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Two fixed-concrete storage slots under one NGRP. Created S1 then S2 so S1.ID < S2.ID —
	// the canonical (node-ID) claim order is therefore [S1, S2].
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP type")
	grp := &nodes.Node{Name: "ABBA-NGRP", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create NGRP")
	s1 := &nodes.Node{Name: "ABBA-S1", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(s1), "create S1")
	s2 := &nodes.Node{Name: "ABBA-S2", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(s2), "create S2")
	if s1.ID >= s2.ID {
		t.Fatalf("fixture invariant: want S1.ID < S2.ID, got S1=%d S2=%d", s1.ID, s2.ID)
	}

	// Order A has already won the LOW-ID slot S1 — standing in for the decisive interleave of
	// the concurrent race (A grabbed S1 first).
	orderA := &orders.Order{EdgeUUID: "abba-A", StationID: "line-1", OrderType: OrderTypeComplex, Status: StatusSourcing, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(orderA), "create order A")
	testutil.MustNoErr(t, db.ClaimSlot(s1.ID, orderA.ID), "A claims S1")

	// Order B drops at S2 THEN S1 — the opposite of node-ID order.
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
		t.Fatal("order B dispatched; expected a slot-claim requeue (non-nil error)")
	}

	// Canonical order: B failed on S1 (low ID) FIRST, so it never claimed S2.
	s2n, _ := db.GetNode(s2.ID)
	if s2n.ClaimedBy != nil {
		t.Fatalf("S2 claimed_by=%d — B grabbed the far slot; canonical ordering must make it fail on S1 first (ABBA)", *s2n.ClaimedBy)
	}
	// S1 is still A's, untouched by B.
	s1n, _ := db.GetNode(s1.ID)
	if s1n.ClaimedBy == nil || *s1n.ClaimedBy != orderA.ID {
		t.Fatalf("S1 claimed_by=%v, want A=%d", s1n.ClaimedBy, orderA.ID)
	}
	// Slots-before-bins: B returned at the slot loop, so it holds NO bin reservations.
	if held, _ := db.ListReservationsByOrder(orderB.ID); len(held) != 0 {
		t.Fatalf("order B holds %d bin reservation(s) — bins must not be touched before slots are secured (SLOTS-BEFORE-BINS)", len(held))
	}
	// B is requeued (not terminal), free to retry once A delivers and releases its slots.
	gotB, _ := db.GetOrder(orderB.ID)
	if gotB.Status != StatusQueued && gotB.Status != StatusSourcing {
		t.Errorf("order B status=%q, want queued/sourcing (requeued to retry)", gotB.Status)
	}
}
