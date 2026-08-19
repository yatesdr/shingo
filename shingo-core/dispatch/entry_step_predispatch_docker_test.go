//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// entry_step_predispatch_docker_test.go — wait_index 0 means two things, and the
// gate believed the wrong one.
//
// ── THE SPECIMEN, FROM THE LANE-STRESS RIG 2026-08-13 ─────────────────────
//
// Order 32 was a two-robot swap:
//
//	0 pickup LSC_023   ← fetch the fresh bin OUT OF STORAGE
//	1 dropoff SLN_007
//	2 wait ALN_005     ← meet the line
//	3 pickup ALN_005   ← take the spent bin off the machine
//	...
//
// It sat in `sourcing`, never dispatched, wait_index 0 — the column's zero value,
// because it had not reached any wait. laneEntryAfterWait read that 0 as "parked
// at wait number 0", found the first wait at step 2, and answered with step 3.
// So the gate asked "is order 32's pickup inside LS_C7" about the bin AT THE
// MACHINE. It is at ALN_005, a station; no station is inside a lane; the answer
// was ErrPickupNotInLane, and the order was refused entry to the lane holding the
// bin it was actually coming for.
//
// FIVE DEMANDS WERE STUCK THIS WAY for the whole 17-minute window, each re-driven
// and re-refused several times a second, under a cause with no releaser: nothing
// that can happen in a plant moves a machine-side bin into a storage lane.
//
// AND THE RIGHT ANSWER WAS ALREADY RECORDED. Order 32's junction said step 0 is
// bin 42, standing at LSC_023 — inside the very lane it was being refused from.
// The lookup was fine; the step index handed to it was not.
//
// ── WHY THIS TEST USES BOTH JUNCTION ROWS ─────────────────────────────────
//
// A fixture with only step 0's row would pass with the bug still in: binForStep
// would find no row for step 3 and fall through to order.BinID. The bug is
// specifically that the WRONG STEP is asked, so both steps must be answerable and
// the two answers must be different bins in different places.
//
// MUTATION: replace entryStepIndex's IsPreDispatch arm with the plain
// laneEntryAfterWait call. The assertion fires with the station bin — which is
// the rig's own refusal, reproduced.
func TestWantedBin_PreDispatchSwapWantsItsFirstStepNotThePostWaitPickup(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, lane, _, _, slots, _, bp := setupDwellGroup(t, db, "ENTRYSTEP", 2, false)

	// The two ends of the swap: a bin in storage, and a bin at the machine.
	fresh := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "ENTRYSTEP-FRESH")

	station := &nodes.Node{Name: "ENTRYSTEP-ALN", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(station), "create the line station")
	spent := createTestBinAtNode(t, db, bp.Code, station.ID, "ENTRYSTEP-SPENT")

	// A SWAP THAT HAS NOT BEEN DISPATCHED. bin_id is the machine-side bin, which
	// is what the column carries for a complex order — the bin claimed at the
	// process node — and is exactly why it is the wrong thing to check a storage
	// pickup against.
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "entrystep-swap"
		o.OrderType = OrderTypeComplex
		o.PayloadCode = bp.Code
		o.Status = protocol.StatusSourcing
		o.SourceNode = slots[0].Name
		o.BinID = &spent.ID
		o.WaitIndex = 0
		o.StepsJSON = `[{"action":"pickup","node":"` + slots[0].Name + `"},` +
			`{"action":"dropoff","node":"` + station.Name + `-STG"},` +
			`{"action":"wait","node":"` + station.Name + `"},` +
			`{"action":"pickup","node":"` + station.Name + `"},` +
			`{"action":"dropoff","node":"` + slots[1].Name + `"}]`
	})
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, fresh.ID, 0, "pickup", slots[0].Name, ""),
		"junction row for the storage fetch")
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, spent.ID, 3, "pickup", station.Name, ""),
		"junction row for the machine pickup")

	got, err := d.wantedBin(order)
	testutil.MustNoErr(t, err, "wantedBin")
	if !got.known {
		t.Fatal("wantedBin does not know which bin this order's lane entry is for, with a junction " +
			"row for every pickup in the plan")
	}
	if got.binID != fresh.ID {
		t.Fatalf("wantedBin says bin %d, want %d — the bin at %s, which is step 0 of this order's own "+
			"plan. Bin %d is the SPENT bin at the machine, which is where it belongs and where it will "+
			"stay until this order gets there. Checking a storage pickup against it asks whether a "+
			"station is inside a lane, which is never true, so the order is refused entry to the lane "+
			"holding the bin it is coming for and nothing in the world can release it",
			got.binID, fresh.ID, slots[0].Name, spent.ID)
	}

	// AND THE GATE AGREES, which is the assertion that actually matters — wantedBin
	// is only interesting because pickupSlotNow acts on it.
	slot, _, err := d.pickupSlotNow(order, lane)
	testutil.MustNoErr(t, err, "pickupSlotNow for the un-dispatched swap")
	if slot == nil || slot.ID != slots[0].ID {
		t.Fatalf("the gate located this order's pickup at %v, want %s. This is the refusal five "+
			"demands sat under for a whole window", slot, slots[0].Name)
	}

	// ── AND A DISPATCHED ORDER STILL READS ITS WAIT ──────────────────────────
	//
	// The fix must not turn into "always use step 0". An order standing at a mark
	// HAS reached a wait, its wait_index means what it says, and its next entry is
	// the work on the far side of that wait — which is the whole reason
	// laneEntryAfterWait exists.
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(protocol.StatusStaged), ""), "stage the order")
	staged, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload the staged order")
	after, err := d.wantedBin(staged)
	testutil.MustNoErr(t, err, "wantedBin for the staged order")
	if after.binID != spent.ID {
		t.Fatalf("a STAGED order's lane entry resolved to bin %d, want the machine-side bin %d. It is "+
			"standing at its wait, so its next entry is the pickup after that wait — reading step 0 "+
			"there would send it back for a bin it is already carrying", after.binID, spent.ID)
	}
}
