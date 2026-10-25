//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// rule1_invariant_docker_test.go — the pending/queued hard-claim invariant, as
// a CI fence. The forbidigo lint guard is selector-match only and blind to raw
// SQL (a bare `UPDATE bins SET claimed_by` writer would slip past it), so this
// sweep is the real fence: NO non-compound order still acquiring
// (pending/queued) holds a HARD bin or slot claim. Under Rule 1 an order
// soft-holds its bin and slot (pending reservations) while it waits and
// hard-claims both only in its hand, at dispatch.
//
// THE SWEEP DRIVES THE REAL SCANNER. It used to build a dispatcher, throw it
// away, and sweep an order nothing had acted on, which proved nothing: the
// order held no claim because no code had run. Here a pass of the engine's
// scanner sources the order's bin, its hand is refused at the slot and rolls
// back, the order parks holding its bin softly, and only then is every
// acquiring order swept.
//
// Exemption: a compound CHILD (parent_order_id IS NOT NULL). A leg is born
// holding nothing (store.CreateCompoundChildrenTx; its blocker is the dig's by
// the parent's lane) and takes its claim at its hand
// (store.ClaimCompoundLegBinTx), so whether the sweep still needs the exemption
// is the one issue's to judge when the legs are issued through it.

// acquiringHardClaims returns the non-compound orders still acquiring that
// hold a HARD claim on a row of table (bins or nodes, by claimed_by).
func acquiringHardClaims(t *testing.T, db *store.DB, table string) []int64 {
	t.Helper()
	rows, err := db.Query(`
		SELECT o.id
		FROM orders o
		JOIN ` + table + ` x ON x.claimed_by = o.id
		WHERE o.status IN ('pending','queued')
		  AND o.parent_order_id IS NULL`)
	testutil.MustNoErr(t, err, "sweep "+table)
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		testutil.MustNoErr(t, rows.Scan(&id), "scan")
		out = append(out, id)
	}
	testutil.MustNoErr(t, rows.Err(), "rows")
	return out
}

// TestRule1_NoHardClaimWhileSourcing_Sweep: a retrieve whose bin is found and
// whose storage slot another order already claims parks after a real scanner
// pass, its hand refused at the slot and rolled back, holding its bin softly;
// and the sweep finds no acquiring order holding a hard bin or slot claim.
func TestRule1_NoHardClaimWhileSourcing_Sweep(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	grpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "NGRP type")
	grp := &nodes.Node{Name: "R1INV-NGRP", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create NGRP")
	slot := &nodes.Node{Name: "R1INV-SLOT", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")
	src := &nodes.Node{Name: "R1INV-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "create src")
	srcBin := clearBin(t, db, sd.Payload.Code, src.ID, "R1INV-BIN", 10)

	// The slot is another order's, hard: a robot on its way to it. That order
	// is dispatched, so the sweep does not count its claim.
	other := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusDispatched })
	testdb.ClaimSlotForTest(t, db, slot.ID, other.ID)

	order := &orders.Order{
		EdgeUUID: "r1inv-simple", StationID: "edge.r1", OrderType: dispatch.OrderTypeRetrieve,
		Status: protocol.StatusPending, Quantity: 1, PayloadCode: sd.Payload.Code,
		SourceNode: src.Name, DeliveryNode: slot.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create simple order")

	eng := newTestEngine(t, db, simulator.New())
	eng.RunFulfillmentScan()

	got, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload")
	if got.Status != protocol.StatusQueued || dispatch.QueueCause(got.QueueCause) != dispatch.CauseClaimFailed {
		t.Fatalf("after a pass the order is %s (cause %q), want queued on its refused hand; the sweep "+
			"below proves nothing about an order the scanner never reached", got.Status, got.QueueCause)
	}
	var soft int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM reservations
		WHERE order_id=$1 AND bin_id=$2 AND state='pending'`, order.ID, srcBin.ID).Scan(&soft), "its soft hold")
	if soft != 1 {
		t.Fatalf("the parked order holds %d pending reservation(s) on its bin, want 1: it sourced the bin and waits holding it softly", soft)
	}

	if v := acquiringHardClaims(t, db, "bins"); len(v) > 0 {
		t.Errorf("Rule 1 violated: acquiring order(s) %v hold a HARD bin claim (soft until dispatch)", v)
	}
	if v := acquiringHardClaims(t, db, "nodes"); len(v) > 0 {
		t.Errorf("Rule 1 violated: acquiring order(s) %v hold a HARD slot claim (soft until dispatch)", v)
	}
	if b, err := db.GetBin(srcBin.ID); err != nil || b.ClaimedBy != nil {
		t.Errorf("the source bin is hard-claimed while its order waits (%v, %v)", b.ClaimedBy, err)
	}
}

// TestRule1_ACompoundChildIsBornHoldingNothing pins the fact the sweep's
// exemption once rested on, as it is now: a compound child is born holding no
// hard claim. Its blocker is the dig's by the parent's lane, and its claim is
// its hand's (store.ClaimCompoundLegBinTx). The exemption it used to justify
// ("a child takes a raw bin claim at intake") described the tree before C2b.
func TestRule1_ACompoundChildIsBornHoldingNothing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	src := &nodes.Node{Name: "R1EXM-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "create src")
	childBin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, src.ID, "R1EXM-BIN")

	parent := &orders.Order{
		EdgeUUID: "r1exm-parent", StationID: "edge.r1", OrderType: dispatch.OrderTypeComplex,
		Status: protocol.StatusReshuffling, Quantity: 1, PayloadCode: sd.Payload.Code,
		SourceNode: src.Name, DeliveryNode: sd.LineNode.Name, Coordinated: true,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")
	child := &orders.Order{
		EdgeUUID: "r1exm-child", StationID: "edge.r1", OrderType: dispatch.OrderTypeMove,
		Status: protocol.StatusQueued, Quantity: 1,
		SourceNode: src.Name, DeliveryNode: sd.LineNode.Name,
		ParentOrderID: &parent.ID, Sequence: 1, BinID: &childBin.ID,
	}
	_, err := db.CreateCompoundChildren([]store.CompoundChild{{Order: child, BinID: childBin.ID}}, nil)
	testutil.MustNoErr(t, err, "create child")

	if b, err := db.GetBin(childBin.ID); err != nil || b.ClaimedBy != nil {
		t.Errorf("a compound child's bin is hard-claimed at its birth (%v, %v); its claim is its hand's", b.ClaimedBy, err)
	}
}
