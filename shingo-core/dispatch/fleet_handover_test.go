//go:build docker

package dispatch

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
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
// MUTATION: drop the ask CAS's guard (orders.TakeAskTx matching on its id
// alone, without its status and no-ask-open conditions) and the loser takes the
// ask again and creates: this fails with two creates.
func TestHandover_CASLoserNeverCallsTheFleet(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	o := &orders.Order{
		EdgeUUID: "HANDOVER-race", StationUID: "line-1",
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

	if _, err := d.dispatchToFleetCore(first, src, dst, Hand{}); err != nil {
		t.Fatalf("first caller should have won: %v", err)
	}
	if _, err := d.dispatchToFleetCore(second, src, dst, Hand{}); err == nil {
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
		EdgeUUID: "HANDOVER-unclaimable", StationUID: "line-1",
		OrderType: OrderTypeRetrieve, Status: protocol.StatusDelivered, Quantity: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")

	src, err := db.GetNodeByDotName(sd.StorageNode.Name)
	testutil.MustNoErr(t, err, "resolve source")
	dst, err := db.GetNodeByDotName(sd.LineNode.Name)
	testutil.MustNoErr(t, err, "resolve dest")

	if _, err := d.dispatchToFleetCore(o, src, dst, Hand{}); err == nil {
		t.Fatal("an order that cannot be claimed must not report a successful dispatch")
	}
	if n := len(backend.CreateRequests()); n != 0 {
		t.Fatalf("fleet saw %d creates for an order that was never claimed, want 0 — a claim that "+
			"does not land must not commit a robot", n)
	}
}

// TestHandover_CreateFailureClosesTheAskAndLeavesTheOrderWhereItRests
//
// The ask's CAS wins, the fleet refuses. The order never left the status it
// rests in (`dispatched` is the fleet's yes, and there was none), so there is
// nothing to roll back: the handover closes the ask and demotes the hand in one
// transaction and returns the refusal as AskClosed, and the caller disposes of
// the order (a wait, a fail, a cancel). Nothing names a fleet job.
func TestHandover_CreateFailureClosesTheAskAndLeavesTheOrderWhereItRests(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewFailingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	o := &orders.Order{
		EdgeUUID: "HANDOVER-createfail", StationUID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusQueued, Quantity: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")

	src, err := db.GetNodeByDotName(sd.StorageNode.Name)
	testutil.MustNoErr(t, err, "resolve source")
	dst, err := db.GetNodeByDotName(sd.LineNode.Name)
	testutil.MustNoErr(t, err, "resolve dest")

	_, err = d.dispatchToFleetCore(o, src, dst, Hand{})
	if err == nil {
		t.Fatal("a refusing fleet must surface an error")
	}
	if !askClosed(err) {
		t.Errorf("err = %v (%T), want AskClosed: the handover closes the ask and demotes the hand itself", err, err)
	}

	after, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload order")
	if after.Status != StatusQueued || after.AskedVendorOrderID != "" || o.AskedVendorOrderID != "" {
		t.Errorf("status %s, ask %q (in hand %q); want queued, as it rested, with the ask closed",
			after.Status, after.AskedVendorOrderID, o.AskedVendorOrderID)
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
// The fleet accepted the job and the database would not record it: the yes
// write (`dispatched` with the vendor id) failed. Nothing
// can track it (loadActiveOrders selects on a non-empty vendor_order_id), nothing
// can re-track it after a restart, and no later cancel can find it. So it is
// cancelled here, while the id is still in hand.
//
// The failure is injected by renaming the column the yes write targets, so the
// create genuinely succeeds and only the write fails.
func TestHandover_IDWriteFailureCancelsTheFleetOrder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	o := &orders.Order{
		EdgeUUID: "HANDOVER-idfail", StationUID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusQueued, Quantity: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")

	src, err := db.GetNodeByDotName(sd.StorageNode.Name)
	testutil.MustNoErr(t, err, "resolve source")
	dst, err := db.GetNodeByDotName(sd.LineNode.Name)
	testutil.MustNoErr(t, err, "resolve dest")

	// Break ONLY the yes write. The ask's CAS before it and the create both
	// still succeed, which is what makes this the right window.
	backend.SetOnCreate(func() {
		if _, err := db.DB.Exec(`ALTER TABLE orders RENAME COLUMN vendor_order_id TO vendor_order_id_x`); err != nil {
			t.Errorf("break vendor id write: %v", err)
		}
	})

	if _, err := d.dispatchToFleetCore(o, src, dst, Hand{}); err == nil {
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

// TestHandover_ACancelReadBeforeTheYesStopsTheFleetJob: an order that ends
// naming a fleet job has had a stop sent to the fleet for that job.
//
// TWO CALLERS, ONE ORDER, as in TestHandover_CASLoserNeverCallsTheFleet: the
// cancel door and a dispatcher each read the order while it rests `queued`,
// with no fleet job. The dispatcher's hand commits, the fleet takes the create
// and the yes writes `dispatched` with the vendor id. Then the cancel goes on
// from the copy it read, through CancelOrderWithCascade, the door that
// HandleOrderCancel and engine.TerminateOrder each call with the order they
// loaded. No lock stands between a door's read and that call.
//
// RED AT 93bd3cd6 (docket B20). LifecycleService.CancelOrder decides whether to
// stop the fleet job from the vendor id on the copy in its hand, which names
// none, and its ending is a compare-and-swap on "still live"
// (store.TerminalizeOrderWithReason), which a `dispatched` row passes. The yes
// was written, so the handover's arm for an order that ended during its create
// does not run either. The row ends `cancelled` naming a job nobody stopped.
//
// It was seen on GitHub in 3 of 8 runs of the integration scenario
// TestScenario_KeepStagedRecovery_AfterCancelledChangeover, two_robot, moment b
// (Integration run 45497000123 attempts 1 and 2, run 45498975575 attempt 4,
// 2026-11-24): "leg 1's vendor order reads "CREATED" at the fleet, want
// STOPPED". There the copy was HandleOrderCancel's own read, made while a
// fulfillment scan was handing the pair to the fleet.
//
// Skipped unless SHINGO_KNOWN_RED is set, so no gate runs it: the fix is
// product work in the cancel path and waits for its ruling. The fix removes
// the skip.
func TestHandover_ACancelReadBeforeTheYesStopsTheFleetJob(t *testing.T) {
	if os.Getenv("SHINGO_KNOWN_RED") == "" {
		t.Skip("docket B20 red pin, red until a cancel stops the fleet job its ending finds on the row; " +
			"run with SHINGO_KNOWN_RED=1")
	}
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	o := &orders.Order{
		EdgeUUID: "HANDOVER-cancel-before-yes", StationUID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusQueued, Quantity: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")

	// The cancel door's read (HandleOrderCancel's first statement), then the
	// dispatcher's: each sees `queued` and no fleet job.
	env := &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-1"}}
	cancelCopy, ok := d.getOwnedOrder(env, o.EdgeUUID)
	if !ok {
		t.Fatal("the cancel door could not read the order (fixture broken)")
	}
	hand, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "load the dispatcher's copy")

	src, err := db.GetNodeByDotName(sd.StorageNode.Name)
	testutil.MustNoErr(t, err, "resolve source")
	dst, err := db.GetNodeByDotName(sd.LineNode.Name)
	testutil.MustNoErr(t, err, "resolve dest")

	// The hand, the create and the yes.
	if _, err := d.dispatchToFleetCore(hand, src, dst, Hand{}); err != nil {
		t.Fatalf("the dispatcher should have handed the order to the fleet: %v", err)
	}
	if len(backend.CreateRequests()) != 1 {
		t.Fatalf("expected exactly one create, got %d (fixture broken)", len(backend.CreateRequests()))
	}
	job := backend.CreateRequests()[0].OrderID

	// The rest of HandleOrderCancel, from the copy it read before the yes.
	d.CancelOrderWithCascade(cancelCopy, "line-1", "aborted by operator",
		CancelCause{Code: protocol.TermOperatorCancelled})

	after, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload order")
	if after.Status != StatusCancelled || after.VendorOrderID != job {
		t.Fatalf("the row reads %s naming fleet job %q, want cancelled naming %q: this pin is about an "+
			"ending that lands on a row the yes has named (fixture broken)", after.Status, after.VendorOrderID, job)
	}
	for _, stopped := range backend.CancelRequests() {
		if stopped == job {
			return
		}
	}
	t.Errorf("order %d ended cancelled naming fleet job %s, and no stop was sent to the fleet for it "+
		"(stops sent: %v). The job is live at the fleet under an order Core has ended: the cancel must "+
		"stop the job its ending finds on the row, not only the one its caller's copy named",
		o.ID, job, backend.CancelRequests())
}

// TestHandover_ACancelQueuedBehindTheYesStopsTheFleetJob is the rule above with
// both doors live and nothing handed over by the test: HandleOrderCancel does
// its own read, and a dispatcher its own handover.
//
// A transaction of the test's own takes the order's row from inside the create,
// so the two writes that want the row queue behind it in the order they come:
// the handover's yes, then the ending of a cancel that HandleOrderCancel began
// after it. The door's read does not wait for the row, and sees the order as it
// rests: `queued`, no fleet job. The test lets the row go, the yes commits, and
// the ending lands on a `dispatched` row.
//
// RED AT 93bd3cd6 for the reason the pin above gives. This is the pin a narrowed
// window does not pass: a fix that reads the order once more before it decides
// stays red here, because every read made before the ending predates the yes.
// Only the ending itself, or a read after it, can learn of the job. Skipped
// unless SHINGO_KNOWN_RED is set; the fix removes the skip.
func TestHandover_ACancelQueuedBehindTheYesStopsTheFleetJob(t *testing.T) {
	if os.Getenv("SHINGO_KNOWN_RED") == "" {
		t.Skip("docket B20 red pin, red until a cancel stops the fleet job its ending finds on the row; " +
			"run with SHINGO_KNOWN_RED=1")
	}
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	o := &orders.Order{
		EdgeUUID: "HANDOVER-cancel-behind-yes", StationUID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusQueued, Quantity: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")
	hand, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "load the dispatcher's copy")
	src, err := db.GetNodeByDotName(sd.StorageNode.Name)
	testutil.MustNoErr(t, err, "resolve source")
	dst, err := db.GetNodeByDotName(sd.LineNode.Name)
	testutil.MustNoErr(t, err, "resolve dest")

	env := &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-1"}}
	queued := make(chan error, 1)
	cancelDone := make(chan struct{})
	backend.SetOnCreate(func() {
		// Inside the create: the hand has committed and the yes is next.
		hold, err := db.DB.Begin()
		if err == nil {
			_, err = hold.Exec(`SELECT 1 FROM orders WHERE id=$1 FOR UPDATE`, o.ID)
		}
		if err != nil {
			queued <- fmt.Errorf("take the order's row: %w", err)
			return
		}
		go func() {
			err := lockWaiters(db, 1) // the yes waits for the row
			if err == nil {
				go func() {
					d.HandleOrderCancel(env, &protocol.OrderCancel{OrderUUID: o.EdgeUUID, Reason: "aborted by operator"})
					close(cancelDone)
				}()
				err = lockWaiters(db, 2) // the cancel has read the order, and its ending waits behind the yes
			}
			if cerr := hold.Commit(); err == nil {
				err = cerr
			}
			queued <- err
		}()
	})

	if _, err := d.dispatchToFleetCore(hand, src, dst, Hand{}); err != nil {
		t.Fatalf("the dispatcher should have handed the order to the fleet: %v", err)
	}
	testutil.MustNoErr(t, <-queued, "queue the yes and the cancel's ending behind the held row")
	select {
	case <-cancelDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancel door never returned (fixture broken)")
	}
	if len(backend.CreateRequests()) != 1 {
		t.Fatalf("expected exactly one create, got %d (fixture broken)", len(backend.CreateRequests()))
	}
	job := backend.CreateRequests()[0].OrderID

	after, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload order")
	if after.Status != StatusCancelled || after.VendorOrderID != job {
		t.Fatalf("the row reads %s naming fleet job %q, want cancelled naming %q: the yes did not land "+
			"ahead of the ending (fixture broken)", after.Status, after.VendorOrderID, job)
	}
	for _, stopped := range backend.CancelRequests() {
		if stopped == job {
			return
		}
	}
	t.Errorf("order %d ended cancelled naming fleet job %s, and no stop was sent to the fleet for it "+
		"(stops sent: %v). HandleOrderCancel read the order before the yes and ended it after: the "+
		"cancel must stop the job its ending finds on the row", o.ID, job, backend.CancelRequests())
}

// lockWaiters returns once n of db's sessions wait on a lock, the fact each
// step of the held-row pins here waits for, or an error after 10 s.
func lockWaiters(db *store.DB, n int) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got int
		if err := db.DB.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err != nil {
			return err
		}
		if got == n {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d session(s) wait on a lock, want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHandover_ACancelEndingBeforeTheYesStopsTheFleetJob is the two pins above
// in the mirror order: the cancel door reads the order and ends it while the
// fleet is creating the job, so its ending lands before the handover's yes. No
// live fleet job is left: the yes finds no row, and the handover's own arm for an
// order that ended during its create (handoverToFleet, 3b) stops the job and
// closes the ask.
//
// GREEN AT a63768f5 (docket B20), committed before the fix as behaviour the fix
// must not move: in this order the ending's row names no job (the yes never
// landed), so the stop is arm 3b's.
func TestHandover_ACancelEndingBeforeTheYesStopsTheFleetJob(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	o := &orders.Order{
		EdgeUUID: "HANDOVER-cancel-ends-before-yes", StationUID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusQueued, Quantity: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")
	hand, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "load the dispatcher's copy")
	src, err := db.GetNodeByDotName(sd.StorageNode.Name)
	testutil.MustNoErr(t, err, "resolve source")
	dst, err := db.GetNodeByDotName(sd.LineNode.Name)
	testutil.MustNoErr(t, err, "resolve dest")

	// Inside the create: the hand has committed, the fleet holds the job and the
	// yes is next. The whole cancel door runs here: its read (`queued`, the ask
	// open, no fleet job) and its ending.
	env := &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-1"}}
	backend.SetOnCreate(func() {
		d.HandleOrderCancel(env, &protocol.OrderCancel{OrderUUID: o.EdgeUUID, Reason: "aborted by operator"})
	})

	if _, err := d.dispatchToFleetCore(hand, src, dst, Hand{}); err == nil {
		t.Fatal("the handover answered a yes for an order the cancel ended during its create")
	}
	if len(backend.CreateRequests()) != 1 {
		t.Fatalf("expected exactly one create, got %d (fixture broken)", len(backend.CreateRequests()))
	}
	job := backend.CreateRequests()[0].OrderID

	after, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload order")
	if after.Status != StatusCancelled {
		t.Fatalf("the row reads %s, want cancelled: the cancel's ending did not land during the create "+
			"(fixture broken)", after.Status)
	}
	if !slices.Contains(backend.CancelRequests(), job) {
		t.Errorf("order %d ended cancelled while the fleet created job %s, and no stop was sent for it "+
			"(stops sent: %v): the job is live at the fleet under an order Core has ended",
			o.ID, job, backend.CancelRequests())
	}
	if after.VendorOrderID != "" || after.AskedVendorOrderID != "" {
		t.Errorf("the ended row names job %q and ask %q, want neither: the yes must not land on an ended "+
			"order, and the ask closes once the fleet has taken the stop", after.VendorOrderID, after.AskedVendorOrderID)
	}
}

// TestHandover_AParentCancelListedBeforeTheYesStopsTheLegsJob is the rule of the
// pins above for the parent-cancel cascade (cancelCompoundChildren), which lists
// a compound's legs once and then ends them one by one: a leg whose yes lands
// after the list was read has its job stopped too.
//
// A leg of a compound parent is handed to the fleet. Inside its create, a
// transaction of the test's own takes the leg's row and the yes waits for it.
// The parent's cancel is started through CancelOrderWithCascade, the door
// HandleOrderCancel and engine.TerminateOrder share: it ends the parent, lists
// the legs (this one `queued`, its ask open, no fleet job) and its ending of the
// leg waits behind the yes. The test lets the row go; the yes commits, and the
// leg's ending lands on a `dispatched` row.
//
// RED AT a63768f5 (docket B20): the cascade ends each leg through
// LifecycleService.CancelOrder with the copy its list read, and CancelOrder
// decides the stop from that copy. Skipped unless SHINGO_KNOWN_RED is set; the
// fix removes the skip.
func TestHandover_AParentCancelListedBeforeTheYesStopsTheLegsJob(t *testing.T) {
	if os.Getenv("SHINGO_KNOWN_RED") == "" {
		t.Skip("docket B20 red pin, red until a cancel stops the fleet job its ending finds on the row; " +
			"run with SHINGO_KNOWN_RED=1")
	}
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	parent := &orders.Order{
		EdgeUUID: "HANDOVER-cascade-parent", StationUID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusReshuffling, Quantity: 1,
		PayloadCode: sd.Payload.Code, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create the compound parent")
	leg := &orders.Order{
		EdgeUUID: "HANDOVER-cascade-leg", StationUID: "line-1",
		OrderType: OrderTypeMove, Status: StatusQueued, Quantity: 1,
		ParentOrderID: &parent.ID, Sequence: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(leg), "create the leg")
	hand, err := db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "load the dispatcher's copy of the leg")
	src, err := db.GetNodeByDotName(sd.StorageNode.Name)
	testutil.MustNoErr(t, err, "resolve source")
	dst, err := db.GetNodeByDotName(sd.LineNode.Name)
	testutil.MustNoErr(t, err, "resolve dest")

	queued := make(chan error, 1)
	cascaded := make(chan error, 1)
	backend.SetOnCreate(func() {
		// Inside the leg's create: the hand has committed and the yes is next.
		hold, err := db.DB.Begin()
		if err != nil {
			queued <- fmt.Errorf("begin the hold: %w", err)
			return
		}
		if _, err := hold.Exec(`SELECT 1 FROM orders WHERE id=$1 FOR UPDATE`, leg.ID); err != nil {
			queued <- errors.Join(fmt.Errorf("take the leg's row: %w", err), hold.Rollback())
			return
		}
		go func() {
			err := lockWaiters(db, 1) // the yes waits for the leg's row
			if err == nil {
				go func() {
					p, perr := db.GetOrder(parent.ID) // the door's read of the parent
					if perr == nil {
						d.CancelOrderWithCascade(p, "line-1", "aborted by operator",
							CancelCause{Code: protocol.TermOperatorCancelled})
					}
					cascaded <- perr
				}()
				err = lockWaiters(db, 2) // the cascade has listed the legs, and its ending of this one waits behind the yes
			}
			if cerr := hold.Commit(); err == nil {
				err = cerr
			}
			queued <- err
		}()
	})

	if _, err := d.dispatchToFleetCore(hand, src, dst, Hand{}); err != nil {
		t.Fatalf("the dispatcher should have handed the leg to the fleet: %v", err)
	}
	testutil.MustNoErr(t, <-queued, "queue the yes and the cascade's ending of the leg behind the held row")
	select {
	case err := <-cascaded:
		testutil.MustNoErr(t, err, "the cancel door's read of the parent")
	case <-time.After(10 * time.Second):
		t.Fatal("the parent's cancel never returned (fixture broken)")
	}
	if len(backend.CreateRequests()) != 1 {
		t.Fatalf("expected exactly one create, got %d (fixture broken)", len(backend.CreateRequests()))
	}
	job := backend.CreateRequests()[0].OrderID

	gotParent, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload the parent")
	after, err := db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "reload the leg")
	if gotParent.Status != StatusCancelled || after.Status != StatusCancelled || after.VendorOrderID != job {
		t.Fatalf("the parent reads %s and the leg %s naming fleet job %q, want both cancelled and the leg "+
			"naming %q: the yes did not land ahead of the cascade's ending of the leg (fixture broken)",
			gotParent.Status, after.Status, after.VendorOrderID, job)
	}
	if slices.Contains(backend.CancelRequests(), job) {
		return
	}
	t.Errorf("leg %d ended cancelled naming fleet job %s, and no stop was sent to the fleet for it "+
		"(stops sent: %v). The parent's cancel listed its legs before the yes and ended this one after: "+
		"the cascade must stop the job its ending finds on the row", leg.ID, job, backend.CancelRequests())
}

// TestHandover_RequestShapesUnchanged is the pin a careless extraction breaks.
//
// The handover is shared; the REQUESTS are not, and must not become so. A sealed
// two-block transport, a Complete:false plan split at a wait, and a Complete:false
// unsealed waybill ending at a gate point are three different jobs. This asserts
// the plain path still ships the shape it shipped before the extraction: a plan
// that enters no lane (one that does is spliced and gated, since every lane
// carries a mark).
func TestHandover_RequestShapesUnchanged(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	o := &orders.Order{
		EdgeUUID: "HANDOVER-shape", StationUID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusQueued, Quantity: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")

	src, err := db.GetNodeByDotName(sd.StorageNode.Name)
	testutil.MustNoErr(t, err, "resolve source")
	dst, err := db.GetNodeByDotName(sd.LineNode.Name)
	testutil.MustNoErr(t, err, "resolve dest")

	if _, err := d.dispatchToFleetCore(o, src, dst, Hand{}); err != nil {
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
