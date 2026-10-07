//go:build docker

package dispatch

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// c2b_rank_take_pins_docker_test.go — C2b A.3: what a shuffle may take.
//
// Source: SYNTH round 1 Fork 2 (Rule 2 ⚖: FCFS within demand, older wins, the
// loser never steals back; a reshuffle inherits its demand's rank; changeover
// pinned top; the loser drops the hold and records who took it); RULINGS-C2b-P0
// §3 Q2 (the holder on the 0b read, a slot-kind take beside TakeSoftHolds
// writing `slot-taken` naming the taker, DemandRank.Outranks in the walk).
//
// The fixture is C2a pin 2's: one blocker in front of the target, and the only
// free parking slot in the group held by a stranger's waiting order on a
// PENDING slot reservation.

// c2bRankTake builds the fixture and returns the holder and the demand, the
// demand ranked by priority against the holder.
func c2bRankTake(t *testing.T, prefix string, demandPriority int) (*Dispatcher, *store.DB, *orders.Order, *orders.Order, func()) {
	t.Helper()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, _, _, dugSlots, sibSlots, bp := setupDwellGroup(t, db, prefix, 1, false)
	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, prefix+"-BLK")
	target := createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, prefix+"-TGT")
	reserved := sibSlots[0]

	holder := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = strings.ToLower(prefix) + "-holder"
		o.OrderType = OrderTypeComplex
		o.PayloadCode = bp.Code
		o.Status = protocol.StatusQueued
		o.DeliveryNode = reserved.Name
	})
	testutil.MustNoErr(t, db.ReserveSlot(reserved.ID, holder.ID), "the holder's pending slot reservation")

	demand := mkQueuedComplexParent(t, db, strings.ToLower(prefix)+"-demand", bp.Code)
	_, err := db.Exec(`UPDATE orders SET priority = $1 WHERE id = $2`, demandPriority, demand.ID)
	testutil.MustNoErr(t, err, "rank the demand")
	demand, err = db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "reload the demand")

	dig := func() {
		d.handleComplexBuriedOnReplay(demand, &BuriedError{Bin: target, Slot: dugSlots[1], LaneID: dug.ID})
	}
	return d, db, holder, demand, dig
}

func c2bSlotRows(t *testing.T, db *store.DB, orderID int64) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM reservations
		WHERE order_id = $1 AND resource_kind = 'slot' AND state <> 'released'`, orderID).Scan(&n), "count slot rows")
	return n
}

// A changeover's dig outranks a waiting order and the group's only parking slot
// is that order's pending reservation: the dig takes it, explicitly. The holder's
// row is deleted and its wait says who took the slot (`slot-taken`, naming the
// dig), in the same transaction as the leg's claim.
//
// At the base the pool offered no stranger's pending slot whatever the rank (0b,
// owner-blind), so the dig parked under no-shuffle-slot and the holder was
// untouched. Now the walk offers it to a dig that outranks the holder
// (shuffleSlotsFrom), and the leg's claim takes it (store.TakeSlotByRank).
//
// MUTATION: make the walk's offer ignore the holder (Holder == 0 only); the dig
// does not start and the assertions fire.
func TestC2bPin_AChangeoversDigTakesAPendingSlot_TheWaiterIsTold(t *testing.T) {
	t.Parallel()
	d, db, holder, demand, dig := c2bRankTake(t, "C2BR1", 100)
	dig()

	var violations []string
	legs, err := db.ListChildOrders(demand.ID)
	testutil.MustNoErr(t, err, "list the dig's legs")
	if len(legs) == 0 {
		after, gErr := db.GetOrder(demand.ID)
		testutil.MustNoErr(t, gErr, "reload the demand")
		t.Fatalf("A.3: the outranking dig did not start: the demand is %s under %q", after.Status, after.QueueCause)
	}
	// The leg chooses its parking at release, which is when it takes the slot.
	releaseDwell(t, d, db, legs[0])
	if n := c2bSlotRows(t, db, holder.ID); n != 0 {
		violations = append(violations, fmt.Sprintf("the outranked holder still holds %d slot reservation(s)", n))
	}
	h, err := db.GetOrder(holder.ID)
	testutil.MustNoErr(t, err, "reload the holder")
	if h.QueueCause != "slot-taken" || !strings.Contains(h.QueueReason, fmt.Sprint(demand.ID)) {
		violations = append(violations, fmt.Sprintf("the holder's wait is %q (%q), want slot-taken naming order %d",
			h.QueueCause, h.QueueReason, demand.ID))
	}
	if len(violations) != 0 {
		t.Fatalf("A.3: %v", violations)
	}
}

// The same shape with the ranks the other way: the waiter is older and the dig
// has no priority over it. Younger never takes from older (Rule 2 ⚖): the dig
// waits under no-shuffle-slot and the waiter keeps its slot. Green today (the
// pool takes nothing pending) and green after (the rank refuses).
func TestC2bPin_AYoungerDigNeverTakesAnOlderWaitersSlot(t *testing.T) {
	t.Parallel()
	_, db, holder, demand, dig := c2bRankTake(t, "C2BR2", 0)
	dig()

	legs, err := db.ListChildOrders(demand.ID)
	testutil.MustNoErr(t, err, "list the dig's legs")
	if len(legs) != 0 {
		t.Fatalf("a younger dig started (%d leg(s)) on an older waiter's only slot", len(legs))
	}
	if n := c2bSlotRows(t, db, holder.ID); n != 1 {
		t.Fatalf("the older waiter holds %d slot reservation(s), want its 1", n)
	}
	after, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "reload the demand")
	if after.QueueCause != string(CauseNoShuffleSlot) {
		t.Fatalf("the younger dig waits under %q, want %q", after.QueueCause, CauseNoShuffleSlot)
	}
}
