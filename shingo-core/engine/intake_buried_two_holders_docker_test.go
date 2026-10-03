//go:build docker

package engine

import (
	"strconv"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestTwoHoldersThroughIntakeBuriedArm pins the dropoff gate against the one
// claimer that does not run under the fulfillment scanner's scanMu: intake's
// buried arm.
//
// The scanner is the single claim point for a plain order, and scanMu
// serializes its gate → find → claim, so two plain orders to one node cannot
// both pass the gate before either claims. But intake still plans a BURIED
// source itself: planTransport gates capacity (CheckDropoffCapacity), resolves
// the source, and on OutcomeReshuffle calls planBuriedReshuffle → createCompound
// → CreateCompoundChildren, which CLAIMS the retrieve child's bin in its own
// transaction, and that child takes the parent's delivery node. All of it on the
// caller's goroutine, outside scanMu. Compound children are never re-gated on
// capacity (AdvanceCompoundOrder), so the intake gate is the only one B meets.
//
// The interleaving, forced with the scanner's PostFindHook:
//
//  1. Order A (plain retrieve, accessible source) is submitted to line N. Its
//     queue emit runs a scan; the scan's gate passes (N empty, no holders), A's
//     bin is found, A moves to sourcing — and the hook fires, before A claims.
//  2. Inside that window, order B (plain retrieve to the SAME N, its only source
//     buried behind a blocker in a lane with a free shuffle slot) is submitted
//     through HandleOrderRequest. B's intake gate sees no holder (A has only
//     found), the source resolves buried, and the reshuffle compound is created,
//     claiming the target bin for a child delivering to N.
//  3. The hook returns; A soft-reserves and confirms its claim.
//
// B is submitted from a goroutine and awaited inside the hook for a short while.
// At ec11ecbd it finished inside that window: B's intake gate passed, its buried
// arm planned the compound and claimed, and A then claimed too — two holders.
// Since intake stopped planning digs, B's intake only names the burial and
// queues; its queued event runs a scan, which waits on scanMu (held by A's scan)
// and so cannot finish inside the window. A claims, the hook returns, and B's
// scan then meets A's claim at the dropoff gate and parks. Either way, at most
// one order may hold a bin bound for N.
//
// The same holds for every door that calls HandleOrderRequest off the scanner's
// goroutine: the Kafka read loop and the HTTP manual-order door
// (www TestManualOrderDoor_TwoHoldersOnOneNode).
//
// RED at ec11ecbd: A and B's retrieve child both held.
func TestTwoHoldersThroughIntakeBuriedArm(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	// B's world: lane with the target at depth 2 behind a blocker at the mouth,
	// one free shuffle slot in the group. sc.LineNode is N — concrete, parentless,
	// not a storage slot.
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix:     "TWOH",
		NumSlots:   2,
		TargetSlot: 2,
		TargetAge:  2 * time.Hour,
	})
	line := sc.LineNode

	// A's world: a different payload with one accessible bin, so A can never be
	// buried and B's only source is the buried target.
	sd := testdb.SetupStandardData(t, db)
	createTestBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "TWOH-A-SRC")

	eng := newTestEngine(t, db, simulator.New())
	d := eng.Dispatcher()

	bDone := make(chan struct{})
	var hookFired, bInterleaved bool
	var aStatusInHook protocol.Status
	d.SetPostFindHook(func() {
		d.SetPostFindHook(nil) // fire once
		hookFired = true
		if o, err := db.GetOrderByUUID("twoh-a"); err == nil {
			aStatusInHook = o.Status
		}
		go func() {
			defer close(bDone)
			d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
				OrderUUID:    "twoh-b",
				OrderType:    dispatch.OrderTypeRetrieve,
				PayloadCode:  sc.Payload.Code,
				SourceNode:   sc.Grp.Name,
				DeliveryNode: line.Name,
				Quantity:     1,
			})
		}()
		select {
		case <-bDone:
			bInterleaved = true
		case <-time.After(2 * time.Second):
		}
	})

	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "twoh-a",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sd.Payload.Code,
		DeliveryNode: line.Name,
		Quantity:     1,
	})

	if !hookFired {
		t.Fatalf("precondition: the post-find hook never fired — A was not found by a scan, so nothing interleaved")
	}
	select {
	case <-bDone:
	case <-time.After(30 * time.Second):
		t.Fatalf("B never returned after A's scan released scanMu")
	}
	t.Logf("A status inside the hook (after find, before claim): %s; B finished inside the window: %v",
		aStatusInHook, bInterleaved)

	a := testdb.RequireOrder(t, db, "twoh-a")
	b := testdb.RequireOrder(t, db, "twoh-b")
	children, err := db.ListChildOrders(b.ID)
	testutil.MustNoErr(t, err, "list B's compound children")
	t.Logf("A: id=%d status=%s bin=%v vendor=%q", a.ID, a.Status, a.BinID, a.VendorOrderID)
	t.Logf("B: id=%d status=%s children=%d code=%s cause=%s", b.ID, b.Status, len(children), b.QueueCode, b.QueueCause)

	// The holder census, exactly the dropoff gate's population.
	rows, err := db.Query(`SELECT o.id, o.edge_uuid, o.status, o.parent_order_id, b.id, b.label
		FROM orders o JOIN bins b ON b.claimed_by = o.id
		WHERE o.delivery_node = $1 ORDER BY o.id, b.id`, line.Name)
	testutil.MustNoErr(t, err, "holder census")
	defer rows.Close()
	holders := map[int64]bool{}
	for rows.Next() {
		var id, binID int64
		var uuid, status, label string
		var parent *int64
		testutil.MustNoErr(t, rows.Scan(&id, &uuid, &status, &parent, &binID, &label), "scan holder")
		holders[id] = true
		p := "-"
		if parent != nil {
			p = strconv.FormatInt(*parent, 10)
		}
		t.Logf("holder → %s: order %d (%s) status=%s parent=%s claims bin %d (%s)",
			line.Name, id, uuid, status, p, binID, label)
	}
	testutil.MustNoErr(t, rows.Err(), "iterate holders")
	n, err := orders.CountInFlightByDeliveryNode(db.DB, line.Name)
	testutil.MustNoErr(t, err, "gate count")

	if len(holders) != 1 || n != 1 {
		t.Fatalf("%d orders hold a claimed bin bound for %s (gate count %d), want exactly 1. "+
			"A planner claimed for %s outside scanMu while A sat between find and claim. "+
			"Two carriers for one line, and a compound child is never re-gated",
			len(holders), line.Name, n, line.Name)
	}
	if !holders[a.ID] {
		t.Fatalf("A (in the fleet's hands since before B arrived) is not the holder")
	}
	if b.Status == dispatch.StatusReshuffling || len(children) != 0 {
		t.Errorf("B planned a dig onto %s while A holds it (status %q, %d children)", line.Name, b.Status, len(children))
	}
	if b.QueueCode != string(protocol.QueueWaitingForSlot) {
		t.Errorf("B waits under %q (%s), want %q: it is waiting for %s to clear", b.QueueCode, b.QueueCause,
			protocol.QueueWaitingForSlot, line.Name)
	}
}
