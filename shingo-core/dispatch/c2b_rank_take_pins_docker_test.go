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
	"shingocore/store/nodes"
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

// askRankTake is c2bRankTake with the holder a Core ask (the level keeper's
// shape) whose window is the group's only parking position, held from its
// issue (AdmitCoreAsk): a sibling lane's slot, or a flat child of the group
// (flat).
func askRankTake(t *testing.T, prefix string, demandPriority int, flat bool) (*Dispatcher, *store.DB, *orders.Order, *orders.Order, *nodes.Node, func()) {
	t.Helper()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	sibDepth := 1
	if flat {
		sibDepth = 0
	}
	grp, dug, _, park, dugSlots, sibSlots, bp := setupDwellGroup(t, db, prefix, sibDepth, flat)
	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, prefix+"-BLK")
	target := createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, prefix+"-TGT")
	window := park
	if !flat {
		window = sibSlots[0]
	}
	ask, err := d.AdmitCoreAsk(CoreAskSpec{UUIDPrefix: "core-mnt-", OrderType: OrderTypeRetrieveEmpty,
		StationID: "test-core", DeliveryNode: window.Name, PayloadDesc: "empty → " + grp.Name,
		OriginClass: protocol.OriginClassOrphan})
	testutil.MustNoErr(t, err, "the ask")

	demand := mkQueuedComplexParent(t, db, strings.ToLower(prefix)+"-demand", bp.Code)
	_, err = db.Exec(`UPDATE orders SET priority = $1 WHERE id = $2`, demandPriority, demand.ID)
	testutil.MustNoErr(t, err, "rank the demand")
	demand, err = db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "reload the demand")
	dig := func() {
		d.handleComplexBuriedOnReplay(demand, &BuriedError{Bin: target, Slot: dugSlots[1], LaneID: dug.ID})
	}
	return d, db, ask, demand, window, dig
}

// TestDig_TakesAnAsksWindowOnlyByRank is the asks' window check as the owner restated it
// (2026-10-25, "rank is the rule"): an ask's held window is a pending slot row
// like every other, so a dig that outranks the ask takes it out loud (the ask's
// row deleted, the ask told slot-taken naming the dig, as every pending slot
// row's holder is: store.takeSlotTx, nodes.TakeableSlotsInGroup), and a dig that
// does not outrank it does not. Then the ask's next pass (its acquire's
// destination step, ReserveStorageDropoff), what "the ask re-chooses at its next
// pass" comes to in the tree: on a lane slot of a group it waits on the slot and
// is aimed back at its group (plainGroupDrop's back-to-group), so the pass after
// chooses again; on a flat child of a group nothing re-aims it, and it waits at
// its window.
//
// At the base the ask held nothing, so its window was free to every dig, no
// rank asked: the dig parked on it and the ask was never told.
func TestDig_TakesAnAsksWindowOnlyByRank(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, prefix string
		flat         bool
	}{{"a lane slot", "AKRL", false}, {"a group's flat child", "AKRF", true}} {
		t.Run("an outranking dig takes "+c.name+" out loud", func(t *testing.T) {
			t.Parallel()
			d, db, ask, demand, window, dig := askRankTake(t, c.prefix, 100, c.flat)
			dig()
			legs, err := db.ListChildOrders(demand.ID)
			testutil.MustNoErr(t, err, "the dig's legs")
			if len(legs) == 0 {
				after, gErr := db.GetOrder(demand.ID)
				testutil.MustNoErr(t, gErr, "reload the demand")
				t.Fatalf("the outranking dig did not start: the demand is %s under %q", after.Status, after.QueueCause)
			}
			leg := releaseDwell(t, d, db, legs[0])
			if leg.DeliveryNode != window.Name {
				t.Fatalf("the leg parks at %q, want the ask's window %s (the group's only parking)", leg.DeliveryNode, window.Name)
			}
			if n := c2bSlotRows(t, db, ask.ID); n != 0 {
				t.Errorf("the outranked ask still holds %d slot row(s)", n)
			}
			told, err := db.GetOrder(ask.ID)
			testutil.MustNoErr(t, err, "reload the ask")
			if told.QueueCause != "slot-taken" || !strings.Contains(told.QueueReason, fmt.Sprint(demand.ID)) {
				t.Fatalf("the ask's wait is %q (%q), want slot-taken naming order %d", told.QueueCause, told.QueueReason, demand.ID)
			}

			// Its next pass.
			dest := d.ReserveStorageDropoff(told)
			if !dest.Refused() || dest.Cause != CauseStoreSlotContended {
				t.Fatalf("the ask's next pass: refused=%v cause %q, want a wait on the slot the dig holds", dest.Refused(), dest.Cause)
			}
			after, err := db.GetOrder(ask.ID)
			testutil.MustNoErr(t, err, "reload the ask")
			if c.flat && after.DeliveryNode != window.Name {
				t.Errorf("the ask on a flat window is aimed at %q, want still %s: nothing re-aims it", after.DeliveryNode, window.Name)
			}
			if !c.flat && after.DeliveryNode == window.Name {
				t.Errorf("the ask on a lane slot is still aimed at %s, want it back at its group to choose again", window.Name)
			}
		})
	}
	t.Run("a dig that does not outrank the ask leaves its window", func(t *testing.T) {
		t.Parallel()
		_, db, ask, demand, window, dig := askRankTake(t, "AKRY", 0, false)
		dig()
		if legs, err := db.ListChildOrders(demand.ID); err != nil || len(legs) != 0 {
			t.Fatalf("a younger dig started (%d leg(s), %v) on the ask's window %s, the group's only parking", len(legs), err, window.Name)
		}
		if n := c2bSlotRows(t, db, ask.ID); n != 1 {
			t.Errorf("the ask holds %d slot row(s), want its window's 1", n)
		}
		after, err := db.GetOrder(demand.ID)
		testutil.MustNoErr(t, err, "reload the demand")
		if after.QueueCause != string(CauseNoShuffleSlot) {
			t.Errorf("the younger dig waits under %q, want %q", after.QueueCause, CauseNoShuffleSlot)
		}
	})
}
