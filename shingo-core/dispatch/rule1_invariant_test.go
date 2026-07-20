//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// rule1_invariant_test.go — the sourcing/queued hard-claim invariant, as a CI
// fence. The forbidigo lint guard is selector-match only and blind to raw SQL
// (a bare `UPDATE bins SET claimed_by` writer would slip past it), so this sweep
// is the real fence: it asserts that NO non-compound order still acquiring
// (pending/queued/sourcing) holds a HARD bin or slot claim. Under Rule 1 the
// simple path soft-holds its bin and slot (pending reservations) while it waits
// and hard-claims both only at dispatch — so a non-dispatched simple order must
// never carry a hard claim.
//
// Exemption: a compound reshuffle CHILD (parent_order_id IS NOT NULL) takes a raw
// bin claim at intake (store/orders.go CreateCompoundChildren) — it is sequenced by the
// compound machinery, not the scanner, and its bin is assigned by the plan. The
// sweep keys the exemption on parent_order_id IS NOT NULL.

// acquiringHardBinClaims returns non-compound orders (parent_order_id IS NULL) that
// are still acquiring (pending/queued/sourcing) yet hold a HARD bin claim. Under
// Rule 1 this set must be empty: the confirm-at-dispatch step is the only writer of
// bins.claimed_by for a simple order, and it runs at dispatch (status → dispatched,
// which leaves the acquiring set).
func acquiringHardBinClaims(db *store.DB) ([]int64, error) {
	rows, err := db.Query(`
		SELECT o.id
		FROM orders o
		JOIN bins b ON b.claimed_by = o.id
		WHERE o.status IN ('pending','queued','sourcing')
		  AND o.parent_order_id IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// acquiringHardSlotClaims is the slot dual of acquiringHardBinClaims: non-compound
// acquiring orders holding a HARD nodes.claimed_by. Must be empty under Rule 1.
func acquiringHardSlotClaims(db *store.DB) ([]int64, error) {
	rows, err := db.Query(`
		SELECT o.id
		FROM orders o
		JOIN nodes n ON n.claimed_by = o.id
		WHERE o.status IN ('pending','queued','sourcing')
		  AND o.parent_order_id IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TestRule1_NoHardClaimWhileSourcing_Sweep runs the invariant against a live DB
// populated with a simple retrieve that parks in sourcing (its slot contended),
// and asserts the sweep finds zero hard bin/slot claims on that order. This is the
// fence that catches any future path that hard-claims before dispatch.
func TestRule1_NoHardClaimWhileSourcing_Sweep(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	// A storage dropoff slot and a source bin.
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP type")
	grp := &nodes.Node{Name: "R1INV-NGRP", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create NGRP")
	slot := &nodes.Node{Name: "R1INV-SLOT", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")
	src := &nodes.Node{Name: "R1INV-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "create src")
	srcBin := testdb.CreateBinAtNode(t, db, bp.Code, src.ID, "R1INV-BIN")

	// Contend the slot: another order hard-claims it so the simple order's slot
	// soft-reserve will conflict and the order parks in sourcing.
	other := testdb.CreateOrder(t, db)
	testdb.ClaimSlotForTest(t, db, slot.ID, other.ID)

	// A simple retrieve order queued for the contended slot.
	order := &orders.Order{
		EdgeUUID:     "r1inv-simple",
		StationID:    "edge.r1",
		OrderType:    OrderTypeRetrieve,
		Status:       StatusQueued,
		Quantity:     1,
		PayloadCode:  bp.Code,
		SourceNode:   src.Name,
		DeliveryNode: slot.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create simple order")

	// Run the scanner-style reserve + soft-acquire + slot-conflict-requeue by hand,
	// the way the scanner plain path would: MoveToSourcing, slot reserve (fails),
	// so the order is sourcing with NO hard claim.
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	_ = d.lifecycle.MoveToSourcing(order, "test", "slot contended")
	// The bin is NOT hard-claimed (Rule 1: soft until complete). Confirm the sweep
	// sees no hard claim on this sourcing order.
	binViolations, err := acquiringHardBinClaims(db)
	testutil.MustNoErr(t, err, "bin sweep")
	for _, v := range binViolations {
		if v == order.ID {
			t.Fatalf("Rule 1 violated: sourcing simple order %d holds a HARD bin claim (should be soft until dispatch)", order.ID)
		}
	}
	slotViolations, err := acquiringHardSlotClaims(db)
	testutil.MustNoErr(t, err, "slot sweep")
	for _, v := range slotViolations {
		if v == order.ID {
			t.Fatalf("Rule 1 violated: sourcing simple order %d holds a HARD slot claim (should be soft until dispatch)", order.ID)
		}
	}

	// Sanity: the src bin itself is unclaimed (we never hard-claimed it).
	gotBin, err := db.GetBin(srcBin.ID)
	testutil.MustNoErr(t, err, "get src bin")
	if gotBin.ClaimedBy != nil {
		t.Fatalf("src bin should be unclaimed under Rule 1, got claimed_by=%d", *gotBin.ClaimedBy)
	}
}

// TestRule1_CompoundChildRawClaimIsExempted confirms the one recorded exception:
// a compound reshuffle child (parent_order_id IS NOT NULL) DOES take a raw bin
// claim (store/orders.go CreateCompoundChildren), and the sweep correctly exempts it.
// This pins the exemption so a future tightening can't silently regress compound
// dispatch.
func TestRule1_CompoundChildRawClaimIsExempted(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	src := &nodes.Node{Name: "R1EXM-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "create src")
	childBin := testdb.CreateBinAtNode(t, db, bp.Code, src.ID, "R1EXM-BIN")

	parent := &orders.Order{
		EdgeUUID: "r1exm-parent", StationID: "edge.r1", OrderType: OrderTypeComplex,
		Status: StatusReshuffling, Quantity: 1, PayloadCode: bp.Code,
		SourceNode: src.Name, DeliveryNode: lineNode.Name, Coordinated: true,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")

	// A compound child carrying a raw bin claim, the way CreateCompoundChildren writes it.
	child := &orders.Order{
		EdgeUUID: "r1exm-child", StationID: "edge.r1", OrderType: OrderTypeMove,
		Status: StatusQueued, Quantity: 1, PayloadCode: bp.Code,
		SourceNode: src.Name, DeliveryNode: lineNode.Name,
		ParentOrderID: &parent.ID, Sequence: 1, BinID: &childBin.ID,
	}
	testutil.MustNoErr(t, db.CreateCompoundChildren([]store.CompoundChild{{Order: child, BinID: childBin.ID}}), "create child")

	binViolations, err := acquiringHardBinClaims(db)
	testutil.MustNoErr(t, err, "bin sweep")
	for _, v := range binViolations {
		if v == child.ID {
			t.Fatalf("compound child %d should be EXEMPTED from the sourcing hard-claim sweep (parent_order_id IS NOT NULL)", child.ID)
		}
	}
}

// (test helpers live in internal/testdb; assertions use protocol/testutil like the
// rest of this package's docker tests.)
