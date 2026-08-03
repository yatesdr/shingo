package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/orders"
)

// order_projection_test.go — the projection applier, tested from the reconcile
// end first.
//
// The healing path is not a rare repair. The Core → Edge outbox drops a message
// permanently once it exhausts its retries, so a projection that never arrives
// is an ordinary outcome, and an order Core authored being absent here is the
// NORMAL state the reconcile exists to correct. Testing the push and treating
// the heal as a follow-up would get the priority exactly backwards.

func projectionFixture(uuid, delivery string) protocol.OrderProjection {
	return protocol.OrderProjection{
		OrderUUID:     uuid,
		OrderType:     protocol.OrderTypeRetrieveEmpty,
		Status:        "queued",
		StationID:     "edge.line1",
		Quantity:      1,
		SourceNode:    "EMPTY-MARKET",
		DeliveryNode:  delivery,
		RetrieveEmpty: true,
		QueueReason:   "waiting for a carrier",
		QueueCode:     "no_bin",
	}
}

// TestReconcile_HealsAnOrderTheEdgeNeverHeardOf is the load-bearing case: Core
// authored an order, the projection did not arrive, and the Edge learns about it
// from the reconcile instead. Before this the order would be invisible on the
// board for its whole life.
func TestReconcile_HealsAnOrderTheEdgeNeverHeardOf(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)

	if _, err := eng.db.GetOrderByUUID("healed-1"); err == nil {
		t.Fatal("fixture: the Edge already knows this order; the test proves nothing")
	}

	eng.HandleUnlistedOrders([]protocol.OrderProjection{projectionFixture("healed-1", "W-A")})

	got, err := eng.db.GetOrderByUUID("healed-1")
	if err != nil || got == nil {
		t.Fatalf("the reconcile did not create the row: %v", err)
	}
	if got.AuthoredBy != "core" {
		t.Errorf("authored_by = %q, want \"core\"", got.AuthoredBy)
	}
	if string(got.Status) != "queued" {
		t.Errorf("status = %q, want queued", got.Status)
	}
	if got.SourceNode != "EMPTY-MARKET" || got.DeliveryNode != "W-A" {
		t.Errorf("nodes = %q -> %q, want EMPTY-MARKET -> W-A", got.SourceNode, got.DeliveryNode)
	}
	if got.QueueReason != "waiting for a carrier" {
		t.Errorf("queue reason = %q; a projected queued order must explain its own wait", got.QueueReason)
	}
}

// TestReconcile_HealingIsIdempotent pins the property everything else rests on.
// A projection can arrive twice — pushed once and healed once, or healed on
// every reconcile — and the second arrival must change nothing. Anything else
// means a duplicate row or a failed reconcile on a healthy plant.
func TestReconcile_HealingIsIdempotent(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	p := projectionFixture("healed-2", "W-B")

	created, err := eng.ApplyOrderProjection(p)
	if err != nil || !created {
		t.Fatalf("first apply: created=%v err=%v, want created", created, err)
	}
	created, err = eng.ApplyOrderProjection(p)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if created {
		t.Error("the second apply reported a new row; the same order arrived twice and became two")
	}

	all, err := orders.List(eng.db.DB)
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}
	var n int
	for _, o := range all {
		if o.UUID == "healed-2" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d rows for one order uuid, want 1", n)
	}
}

// TestProjection_UpdatesAnExistingRowWithoutErasingEdgeState pins what an update
// may and may not touch. Core's view of an order does not include the working
// state the Edge accumulates by doing the job, so re-projecting must not blank
// it — a status refresh that erased the bin binding would break the very
// delivery it was refreshing.
func TestProjection_UpdatesAnExistingRowWithoutErasingEdgeState(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	p := projectionFixture("healed-3", "W-C")
	if _, err := eng.ApplyOrderProjection(p); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	row, err := eng.db.GetOrderByUUID("healed-3")
	if err != nil || row == nil {
		t.Fatalf("read back: %v", err)
	}
	// The Edge learns the bin while working the order.
	binID := int64(4242)
	if err := orders.UpdateBinID(eng.db.DB, row.ID, &binID); err != nil {
		t.Fatalf("set bin: %v", err)
	}

	// Core re-projects with a newer status.
	p.Status = "in_transit"
	p.QueueReason = ""
	p.QueueCode = ""
	if _, err := eng.ApplyOrderProjection(p); err != nil {
		t.Fatalf("re-apply: %v", err)
	}

	got, err := eng.db.GetOrderByUUID("healed-3")
	if err != nil || got == nil {
		t.Fatalf("read back after re-apply: %v", err)
	}
	if string(got.Status) != "in_transit" {
		t.Errorf("status = %q, want in_transit — the update did not land", got.Status)
	}
	if got.QueueReason != "" {
		t.Errorf("queue reason = %q, want cleared — the order is moving now", got.QueueReason)
	}
	if got.BinID == nil || *got.BinID != binID {
		t.Errorf("bin id = %v, want %d — re-projecting erased what the Edge learned by doing the work", got.BinID, binID)
	}
}

// TestProjection_WritesNothingToTheOutbox is the provenance guard.
//
// An Edge that answered a projection by telling Core about the order would loop.
// The absence of a sender call is asserted rather than assumed, because it is
// the kind of property a later refactor breaks by accident — routing the
// projection through the ordinary create path would look like a tidy-up and
// would start the loop.
func TestProjection_WritesNothingToTheOutbox(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)

	before := outboxCount(t, eng)
	if _, err := eng.ApplyOrderProjection(projectionFixture("healed-4", "W-D")); err != nil {
		t.Fatalf("apply: %v", err)
	}
	eng.HandleUnlistedOrders([]protocol.OrderProjection{projectionFixture("healed-5", "W-E")})
	after := outboxCount(t, eng)

	if after != before {
		t.Errorf("outbox grew from %d to %d applying projections; a projected order must never be reported back to Core", before, after)
	}
}

func outboxCount(t *testing.T, eng *Engine) int {
	t.Helper()
	var n int
	if err := eng.db.DB.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// TestProjection_UnresolvableDeliveryNodeStillLandsTheRow pins the shape a
// stricter applier would get wrong. A Core node with no matching Edge process
// node is legitimate — a storage location, or a node belonging to another
// station — and refusing the projection over it would throw the whole order away
// to avoid a nullable column doing its job.
func TestProjection_UnresolvableDeliveryNodeStillLandsTheRow(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	p := projectionFixture("healed-6", "A-NODE-THIS-EDGE-HAS-NEVER-HEARD-OF")

	created, err := eng.ApplyOrderProjection(p)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !created {
		t.Fatal("no row created")
	}
	got, err := eng.db.GetOrderByUUID("healed-6")
	if err != nil || got == nil {
		t.Fatalf("read back: %v", err)
	}
	if got.ProcessNodeID != nil {
		t.Errorf("process node = %v, want nil — there is no Edge node for that name", got.ProcessNodeID)
	}
	if got.DeliveryNode != "A-NODE-THIS-EDGE-HAS-NEVER-HEARD-OF" {
		t.Errorf("delivery node = %q; the name Core sent must survive even when it resolves to nothing here", got.DeliveryNode)
	}
}

// TestProjection_RefusesAnOrderWithNoIdentity: a projection with no UUID cannot
// be upserted (there is nothing to be idempotent on) and must not silently
// create a row that no later projection can ever match.
func TestProjection_RefusesAnOrderWithNoIdentity(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	if _, err := eng.ApplyOrderProjection(protocol.OrderProjection{DeliveryNode: "W-A"}); err == nil {
		t.Error("accepted a projection with no uuid")
	}
}
