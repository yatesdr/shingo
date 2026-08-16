//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestHandover_CASLoserNeverCallsTheFleet is the reason this ordering changed.
//
// Two callers resolve to one order. Under the old create-then-record sequence
// BOTH called CreateOrder and the loser then cancelled its own vendor order —
// the old comment said so: "the robot is already committed — CreateOrder
// succeeded above." A robot was spent and then chased.
//
// The assertion is on the FLEET, not on the order row: exactly one create, ever.
// Asserting "one of them returned an error" would pass under the old ordering
// too, which is the whole point of putting the count on the backend.
//
// MUTATION: move the lifecycle.Dispatch call in handoverToFleet back below
// CreateOrder and this fails with two creates.
func TestHandover_CASLoserNeverCallsTheFleet(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	o := &orders.Order{
		EdgeUUID: "HANDOVER-race", StationID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusQueued, Quantity: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")

	// TWO CALLERS, ONE ORDER — modelled the way two racing callers actually
	// differ: each holds its OWN snapshot of the row, both reading `queued`.
	// This is exactly what GetOrder gives two goroutines that loaded before
	// either wrote, without depending on them interleaving.
	first, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "load snapshot one")
	second, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "load snapshot two")

	src, err := db.GetNodeByDotName(sd.StorageNode.Name)
	testutil.MustNoErr(t, err, "resolve source")
	dst, err := db.GetNodeByDotName(sd.LineNode.Name)
	testutil.MustNoErr(t, err, "resolve dest")

	if _, err := d.dispatchToFleetCore(first, src, dst); err != nil {
		t.Fatalf("first caller should have won: %v", err)
	}
	if _, err := d.dispatchToFleetCore(second, src, dst); err == nil {
		t.Fatal("second caller was not refused — both callers claimed one order")
	}

	if n := len(backend.CreateRequests()); n != 1 {
		t.Fatalf("fleet saw %d creates for one order, want exactly 1. The losing caller reached the "+
			"fleet before discovering it lost — that is a robot committed and then chased", n)
	}
	// And it did not have to be cancelled, because it was never created.
	if n := len(backend.CancelRequests()); n != 0 {
		t.Errorf("fleet saw %d cancels, want 0 — the loser should never have created anything to cancel", n)
	}
}

// TestHandover_UnclaimableOrderNeverReachesTheFleet pins the abort semantics
// directly, rather than leaving them resting on "no other test failed".
//
// The claim can fail three ways: a lost race, an illegal transition, and a
// database error. Only the first is a race. This covers the second — an order
// whose status does not lead to `dispatched` — and the rule is the same for all
// three: no claim, no fleet order.
//
// It matters because the OLD ordering did the opposite. With the create already
// done, a failed status write left nothing to do but log and carry on; copying
// that tolerance into CAS-first would create a robot job for an order that was
// never claimed. This asserts the reversal.
func TestHandover_UnclaimableOrderNeverReachesTheFleet(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	// `delivered` does not transition to `dispatched` — an illegal edge, not a
	// concurrent one, so it exercises the arm that used to proceed.
	o := &orders.Order{
		EdgeUUID: "HANDOVER-unclaimable", StationID: "line-1",
		OrderType: OrderTypeRetrieve, Status: protocol.StatusDelivered, Quantity: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")

	src, err := db.GetNodeByDotName(sd.StorageNode.Name)
	testutil.MustNoErr(t, err, "resolve source")
	dst, err := db.GetNodeByDotName(sd.LineNode.Name)
	testutil.MustNoErr(t, err, "resolve dest")

	if _, err := d.dispatchToFleetCore(o, src, dst); err == nil {
		t.Fatal("an order that cannot be claimed must not report a successful dispatch")
	}
	if n := len(backend.CreateRequests()); n != 0 {
		t.Fatalf("fleet saw %d creates for an order that was never claimed, want 0 — a claim that "+
			"does not land must not commit a robot", n)
	}
}

// TestHandover_CreateFailureLeavesTheOrderClaimedAndDoesNotRollBack
//
// CAS wins, the fleet refuses. The status is deliberately NOT rolled back:
// re-queueing an order the fleet just refused invites a re-dispatch loop against
// a fleet that is saying no. The order is left `dispatched` for its caller to
// fail, and `dispatched` is a stuck-sweep candidate (protocol.IsStuckSweepCandidate)
// so it is recoverable even if the caller's own write fails.
func TestHandover_CreateFailureLeavesTheOrderClaimedAndDoesNotRollBack(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewFailingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	o := &orders.Order{
		EdgeUUID: "HANDOVER-createfail", StationID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusQueued, Quantity: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")

	src, err := db.GetNodeByDotName(sd.StorageNode.Name)
	testutil.MustNoErr(t, err, "resolve source")
	dst, err := db.GetNodeByDotName(sd.LineNode.Name)
	testutil.MustNoErr(t, err, "resolve dest")

	if _, err := d.dispatchToFleetCore(o, src, dst); err == nil {
		t.Fatal("a refusing fleet must surface an error")
	}

	after, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload order")
	if after.Status != protocol.StatusDispatched {
		t.Errorf("status = %s, want dispatched — the claim stands and the caller disposes of it; "+
			"rolling back here would re-queue against a fleet that just refused", after.Status)
	}
	if after.VendorOrderID != "" {
		t.Errorf("vendor_order_id = %q, want empty — nothing was created, so nothing may claim the "+
			"row names a fleet job (an id here would make the order look tracked and gate-staged)",
			after.VendorOrderID)
	}
}

// TestHandover_IDWriteFailureCancelsTheFleetOrder — the surviving orphan-guard
// cause, and the only one left once the CAS comes first.
//
// The fleet accepted the job and the database would not record its id. Nothing
// can track it (loadActiveOrders selects on a non-empty vendor_order_id), nothing
// can re-track it after a restart, and no later cancel can find it. So it is
// cancelled here, while the id is still in hand.
//
// The failure is injected by dropping the column the id write targets, so the
// create genuinely succeeds and only the write fails.
func TestHandover_IDWriteFailureCancelsTheFleetOrder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	o := &orders.Order{
		EdgeUUID: "HANDOVER-idfail", StationID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusQueued, Quantity: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")

	src, err := db.GetNodeByDotName(sd.StorageNode.Name)
	testutil.MustNoErr(t, err, "resolve source")
	dst, err := db.GetNodeByDotName(sd.LineNode.Name)
	testutil.MustNoErr(t, err, "resolve dest")

	// Break ONLY the id write. The CAS above it and the create before it both
	// still succeed, which is what makes this the right window.
	backend.SetOnCreate(func() {
		if _, err := db.DB.Exec(`ALTER TABLE orders RENAME COLUMN vendor_order_id TO vendor_order_id_x`); err != nil {
			t.Errorf("break vendor id write: %v", err)
		}
	})

	if _, err := d.dispatchToFleetCore(o, src, dst); err == nil {
		t.Fatal("a failed vendor-id write must surface an error")
	}

	if len(backend.CreateRequests()) != 1 {
		t.Fatalf("expected exactly one create, got %d (fixture broken)", len(backend.CreateRequests()))
	}
	cancels := backend.CancelRequests()
	if len(cancels) != 1 {
		t.Fatalf("fleet saw %d cancels, want 1 — a job Core cannot name must be cancelled while its "+
			"id is still in hand; after this returns, nothing can find it again", len(cancels))
	}
	if cancels[0] != backend.CreateRequests()[0].OrderID {
		t.Errorf("cancelled %q, want the order just created (%q)", cancels[0], backend.CreateRequests()[0].OrderID)
	}
}

// TestHandover_RequestShapesUnchanged is the pin a careless extraction breaks.
//
// The handover is shared; the REQUESTS are not, and must not become so. A sealed
// two-block transport, a Complete:false plan split at a wait, and a Complete:false
// unsealed waybill ending at a gate point are three different jobs. This asserts
// the plain path still ships the shape it shipped before the extraction — the one
// every plant actually runs, since no group sets lane_enforcement.
func TestHandover_RequestShapesUnchanged(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	o := &orders.Order{
		EdgeUUID: "HANDOVER-shape", StationID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusQueued, Quantity: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")

	src, err := db.GetNodeByDotName(sd.StorageNode.Name)
	testutil.MustNoErr(t, err, "resolve source")
	dst, err := db.GetNodeByDotName(sd.LineNode.Name)
	testutil.MustNoErr(t, err, "resolve dest")

	if _, err := d.dispatchToFleetCore(o, src, dst); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	reqs := backend.CreateRequests()
	if len(reqs) != 1 {
		t.Fatalf("got %d creates, want 1", len(reqs))
	}
	req := reqs[0]
	if !req.Complete {
		t.Error("plain transport must ship SEALED (Complete:true) — the unsealed shape belongs to the " +
			"gated valves and to complex staging, not here")
	}
	if len(req.Blocks) != 2 {
		t.Errorf("plain transport blocks = %d, want 2 (pickup, dropoff)", len(req.Blocks))
	}
	if req.ExternalID != o.EdgeUUID {
		t.Errorf("ExternalID = %q, want the edge uuid %q", req.ExternalID, o.EdgeUUID)
	}
	if req.OrderID == "" {
		t.Error("OrderID must be the Core-minted vendor id")
	}
}
