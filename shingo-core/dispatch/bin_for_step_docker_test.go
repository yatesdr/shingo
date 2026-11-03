//go:build docker

package dispatch

import (
	"encoding/json"
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// setSteps writes a plan onto an order and returns it reloaded.
func setSteps(t *testing.T, db *store.DB, o *orders.Order, steps []resolvedStep) *orders.Order {
	t.Helper()
	b, err := json.Marshal(steps)
	testutil.MustNoErr(t, err, "marshal steps")
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(o.ID, string(b)), "persist steps")
	reloaded, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload order")
	return reloaded
}

// TestPickupSlot_AsksWhichBinTHISSTEPWants is F-25.
//
// ── THE DEFECT ────────────────────────────────────────────────────────────
//
// The gate asks the right question before opening a lane — "where is the bin
// you are coming in for?" — of the wrong source. It read orders.bin_id, which is
// one column with two meanings: the SOURCE bin for a plain retrieve, and "the
// bin claimed at the process node" for a complex order, i.e. the bin at the
// MACHINE.
//
// So a swap parked at a lane's mark to fetch a FRESH bin from storage was
// checked against the bin at the machine, found to be somewhere else — correctly,
// that is where it belongs — and refused entry. Forever: the answer never
// changes, it arrived as an ERROR rather than a refusal, and a gate-staged order
// is exempt from the abandon sweep. Ten hours on the rig, holding a bin, a robot
// and a lane, with the plant starving behind it.
//
// ── THE FIXTURE IS THE SHAPE THAT BREAKS ──────────────────────────────────
//
// Three conditions have to coincide, and this builds exactly them: more than one
// cart, one of them AT the process node (the one orders.bin_id named), and a
// lane entry that is a PICKUP at a different node.
//
// MUTATIONS RUN (both fire):
//  1. answer wantedBin with the cart at the process node → (a) the storage cart
//     is never found and the entry is refused for a cart it was not coming for.
//  2. return a bare error instead of ErrPickupNotInLane → (c) the verdict is an
//     error rather than a refusal, so the order carries no usable cause and the
//     evaluator can only log and skip it.
func TestPickupSlot_AsksWhichBinTHISSTEPWants(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	lane, _, w, _, bp := clearLaneFixture(t, db, "STEPBIN")
	cell := lineNode(t, db, "STEPBIN-CELL")

	// The FRESH bin, in the lane — what the robot is actually coming in for.
	fresh := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-STEPBIN-FRESH")
	// The ACTIVE bin, at the machine — where it belongs.
	active := createTestBinAtNode(t, db, bp.Code, cell.ID, "BIN-STEPBIN-ACTIVE")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.Status = "staged"
		o.SourceNode = w[0].Name
		o.ProcessNode = cell.Name
	})
	order = setSteps(t, db, order, []resolvedStep{
		{Action: protocol.ActionWait, Node: "STEPBIN-WALL-WAIT", WaitKind: WaitKindLane, WaitLane: lane.ID},
		{Action: protocol.ActionPickup, Node: w[0].Name},  // step 1 — the entry, FRESH
		{Action: protocol.ActionDropoff, Node: cell.Name}, // step 2
		{Action: protocol.ActionPickup, Node: cell.Name},  // step 3 — the ACTIVE bin
		{Action: protocol.ActionDropoff, Node: w[1].Name}, // step 4
	})

	// The order's carts: the fresh one lifted at step 1, the active one at 3.
	testdb.SeedCartRow(t, db, order.ID, fresh.ID, 1, 2)
	testdb.SeedCartRow(t, db, order.ID, active.ID, 3, 4)
	order, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload")

	// (a) THE GATE RESOLVES THE BIN THE ENTRY IS FOR.
	got := wantedBin(order)
	if !got.known {
		t.Fatal("no bin resolved for the lane entry — the gate cannot answer 'where is the thing " +
			"you are coming in for' at all")
	}
	if got.binID != fresh.ID {
		t.Errorf("lane entry resolved to bin %d, want %d (the FRESH bin in the lane), not the "+
			"ACTIVE bin at the machine (%d). The gate must ask the step, not the order",
			got.binID, fresh.ID, active.ID)
	}

	// (b) AND FINDS IT IN THE LANE, so the entry is admissible.
	slot, _, err := d.pickupSlotNow(order, lane)
	if err != nil {
		t.Fatalf("pickupSlotNow: %v — the fresh bin is sitting in %s, which is a slot of this lane; "+
			"an entry that cannot be bound here is the ten-hour wedge", err, w[0].Name)
	}
	if slot.ID != w[0].ID {
		t.Errorf("bound to %s, want %s", slot.Name, w[0].Name)
	}
}

// TestPickupSlot_BinElsewhereIsARefusalNotAnError pins the second half: when the
// answer really is "not here", it must be a fact the caller can refuse on.
//
// A bare error is indistinguishable from a failed read, so the evaluator logs and
// skips — no usable cause on the row, no heal-dig proposal, and no abandon-sweep
// bound, because a gate-staged order is exempt from it by design. That is the
// difference between an order that waits and one that is lost.
func TestPickupSlot_BinElsewhereIsARefusalNotAnError(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	lane, _, w, _, bp := clearLaneFixture(t, db, "STEPBINX")
	away := lineNode(t, db, "STEPBINX-AWAY")

	gone := createTestBinAtNode(t, db, bp.Code, away.ID, "BIN-STEPBINX-GONE")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = "staged"
		o.SourceNode = w[0].Name
	})
	testdb.SeedCartRow(t, db, order.ID, gone.ID, 0, 1)
	order, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload")

	_, _, err = d.pickupSlotNow(order, lane)
	if err == nil {
		t.Fatal("a bin outside the lane was silently accepted")
	}
	if !errors.Is(err, ErrPickupNotInLane) {
		t.Errorf("err = %v, want ErrPickupNotInLane. Without the sentinel the gate cannot tell a "+
			"definite 'the bin is elsewhere' from 'I could not read the lane', so it treats a "+
			"knowable answer as an unknown one and wedges instead of waiting", err)
	}
}

// TestBinForStep_ARelayIsItsRow pins that a relay pickup is answered by its own
// row: a step that re-lifts a cart THIS ORDER set down there itself claims no
// cart, and the walk (domain.Lifts) writes its lift as a row all the same. No
// positional answer is left to give.
func TestBinForStep_ARelayIsItsRow(t *testing.T) {
	t.Parallel()
	o := &orders.Order{
		StepsJSON: `[{"action":"pickup","node":"SRC"},{"action":"dropoff","node":"STAGE"},` +
			`{"action":"pickup","node":"STAGE"},{"action":"dropoff","node":"LINE"}]`,
	}
	o.Carts = domain.Lifts(domain.OrderPlan(o), map[int]int64{0: 41})
	if got := binForStep(o, 2); !got.known || got.binID != 41 {
		t.Fatalf("the relay step's cart = %+v, want bin 41 from its row: the cart this order set down at STAGE", got)
	}
	if got := binForStep(o, 3); !got.known || got.binID != 41 {
		t.Errorf("the relay's set-down = %+v, want bin 41", got)
	}
}
