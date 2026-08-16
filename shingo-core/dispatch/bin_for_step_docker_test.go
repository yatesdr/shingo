//go:build docker

package dispatch

import (
	"encoding/json"
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
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
// claimed bin, one of them AT the process node (so it wins bin_id), and a lane
// entry that is a PICKUP at a different node.
//
// MUTATIONS RUN (both fire):
//  1. point pickupSlotNow back at order.BinID → (a) the storage bin is never
//     found and the entry is refused for a bin it was not coming for.
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
	// The ACTIVE bin, at the machine — where it belongs, and what bin_id names.
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

	// The allocator's rule: bin_id is the bin claimed at the PROCESS NODE.
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, active.ID), "bin_id = the active bin")
	// ...and the per-step truth it records alongside it.
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, fresh.ID, 1, protocol.ActionPickup, w[0].Name, cell.Name), "junction step 1")
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, active.ID, 3, protocol.ActionPickup, cell.Name, w[1].Name), "junction step 3")
	order, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload")

	// (a) THE GATE RESOLVES THE BIN THE ENTRY IS FOR.
	got, err := d.wantedBin(order)
	testutil.MustNoErr(t, err, "wantedBin")
	if !got.known {
		t.Fatal("no bin resolved for the lane entry — the gate cannot answer 'where is the thing " +
			"you are coming in for' at all")
	}
	if got.binID != fresh.ID {
		t.Errorf("lane entry resolved to bin %d, want %d (the FRESH bin in the lane). bin_id names "+
			"the ACTIVE bin at the machine (%d) — right for a status board, wrong for this step. "+
			"The gate must ask the step, not the order", got.binID, fresh.ID, active.ID)
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
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, gone.ID), "bin_id")
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

// TestPickupSlot_UnreadableJunctionIsUndeterminedNotElsewhere is R2-2: the two
// answers that look identical and must not be treated alike.
//
// ── THE TWO CASES ─────────────────────────────────────────────────────────
//
//	"the plan names no bin for this step"  → an ANSWER. Single-bin orders
//	                                          produce it, and order.BinID is the
//	                                          right resolution for them.
//	"I could not read the junction"        → NOT an answer. Nothing may be
//	                                          substituted for it.
//
// binForStep's doc has always said the second one fails closed. It could not:
// it returned a bare stepBin, so both cases arrived at the caller as
// `known: false`, and wantedBin resolved that by falling through to
// order.BinID — which on a multi-bin complex order is the bin at the MACHINE.
// That is F-25's exact wrong source, reached through the error path instead of
// the index path, and it would have refused the entry DEFINITIVELY
// (gate-pickup-elsewhere: "the plant moved it") for a database that was simply
// not answering.
//
// The distinction is worth a test because the two dispositions differ in kind:
// an undetermined candidate is held and re-asked on the next pass, while a
// definite refusal is a fact about the plant that a heal dig may be proposed
// against. Reporting an outage as the latter sends the reader to the floor.
//
// MUTATION (fires): drop the `if err != nil` propagation in wantedBin and return
// `fromOrder` → the error becomes nil, pickupSlotNow reports the bin at the
// machine as outside the lane, and this test fails with err wrapping
// ErrPickupNotInLane.
func TestPickupSlot_UnreadableJunctionIsUndeterminedNotElsewhere(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	lane, _, w, _, bp := clearLaneFixture(t, db, "STEPBINR")
	cell := lineNode(t, db, "STEPBINR-CELL")

	// The F-25 shape: two claimed bins, bin_id naming the one at the MACHINE,
	// and a lane entry that is a pickup of the OTHER one.
	fresh := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-STEPBINR-FRESH")
	active := createTestBinAtNode(t, db, bp.Code, cell.ID, "BIN-STEPBINR-ACTIVE")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = "staged"
		o.SourceNode = w[0].Name
	})
	order = setSteps(t, db, order, []resolvedStep{
		{Action: protocol.ActionWait, Node: "STEPBINR-WALL-WAIT", WaitKind: WaitKindLane, WaitLane: lane.ID},
		{Action: protocol.ActionPickup, Node: w[0].Name},
		{Action: protocol.ActionDropoff, Node: cell.Name},
	})
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, active.ID), "bin_id = the active bin")
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, fresh.ID, 1, protocol.ActionPickup, w[0].Name, cell.Name), "junction step 1")
	order, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload")

	// Precondition: with the junction readable the entry binds cleanly. Without
	// this the test could pass on a fixture that was broken for another reason.
	if _, _, pErr := d.pickupSlotNow(order, lane); pErr != nil {
		t.Fatalf("precondition: the entry should bind while the junction is readable: %v", pErr)
	}

	// Now break the junction read specifically — the same technique
	// TestAdvanceCompoundOrder_SurfacesDBError uses.
	_, err = db.DB.Exec(`ALTER TABLE order_bins RENAME COLUMN step_index TO step_index_broken`)
	testutil.MustNoErr(t, err, "break the junction read")

	_, _, err = d.pickupSlotNow(order, lane)
	if err == nil {
		t.Fatal("an unreadable junction was answered rather than refused. Whatever bin it named, it " +
			"was a guess — and the caller cannot tell a guess from a fact")
	}
	if errors.Is(err, ErrPickupNotInLane) {
		t.Errorf("err = %v, and it carries ErrPickupNotInLane — the DEFINITE answer. The junction "+
			"could not be read, so the gate fell back to order.BinID (the bin at the machine), found "+
			"it outside the lane, and reported a database outage as a bin that moved. That refusal "+
			"never changes and a gate-staged order is exempt from the abandon sweep", err)
	}
}

// TestBinForStep_RelayPickupNeedsNoClaim pins the shape an assertion would have
// broken.
//
// Not every pickup claims a bin, by design: a step that re-picks a bin THIS ORDER
// DROPPED THERE ITSELF needs no claim, and the planner records such steps as
// skips. The bin is whatever the order left at that node, so the node answers it.
// A "every pickup step has a junction row" check would fail every one of these.
func TestBinForStep_RelayPickupNeedsNoClaim(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	order := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "staged" })
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "SRC"},
		{Action: protocol.ActionDropoff, Node: "STAGE"}, // the order puts a bin here
		{Action: protocol.ActionPickup, Node: "STAGE"},  // ...and re-picks it: a RELAY
	}
	got, err := d.binForStep(order, steps, 2)
	testutil.MustNoErr(t, err, "binForStep")
	if !got.known {
		t.Fatal("a relay pickup resolved to nothing. It claims no bin because it does not need one — " +
			"the bin is the one this order dropped at that node — and treating that as unanswerable " +
			"would wedge the entry exactly like the bug this fixes")
	}
	if got.atNode != "STAGE" {
		t.Errorf("relay resolved to node %q, want STAGE — the node IS the answer", got.atNode)
	}
}
