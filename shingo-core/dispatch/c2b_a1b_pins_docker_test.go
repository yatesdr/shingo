//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// TestC2bPin_ADemandCancelledMidDigLeavesNoSoftHold is the soft hold's lifetime
// (C2b A.1b, the orc's condition 4). A coordinated demand's dig soft-reserves the
// bin it uncovers at birth; a demand cancelled while the dig runs must leave that
// row behind it no more than any other hold, and its legs end as they always
// have, through the cancel's cascade.
//
// MUTATION: make TerminalizeOrder skip the order's bin reservations; the row
// assertion fires.
func TestC2bPin_ADemandCancelledMidDigLeavesNoSoftHold(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: "A1BC", NumSlots: 2, NumShuffles: 1})
	d, _ := newTestDispatcherWithResolver(t, db)
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "a1bc-demand", PayloadCode: sc.Payload.Code, Quantity: 1,
		Steps: []protocol.ComplexOrderStep{prPick(sc.Grp.Name), prDrop(sc.LineNode.Name)},
	})
	prScanPass(t, d, db, "a1bc-demand")
	demand := prReloadUUID(t, db, "a1bc-demand")
	if demand.Status != StatusReshuffling {
		t.Fatalf("fixture: the demand is %s (%s), want it digging", demand.Status, demand.QueueCause)
	}
	if n := pendingBinRows(t, db, demand.ID, sc.TargetBin.ID); n != 1 {
		t.Fatalf("fixture: the demand holds %d pending rows on its target at the dig's birth, want 1", n)
	}

	d.HandleOrderCancel(testEnvelope(), &protocol.OrderCancel{OrderUUID: demand.EdgeUUID, Reason: "operator"})

	ended := prReloadUUID(t, db, "a1bc-demand")
	if !protocol.IsTerminal(ended.Status) {
		t.Fatalf("the demand is %s after its cancel, want terminal", ended.Status)
	}
	var rows int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM reservations WHERE order_id = $1`,
		demand.ID).Scan(&rows), "count the demand's reservations")
	if rows != 0 {
		t.Fatalf("the cancelled demand still holds %d reservation rows — its soft hold on bin %d "+
			"outlived it, and the burial guards would keep the slots in front of that bin closed "+
			"for an order that is not coming", rows, sc.TargetBin.ID)
	}
	legs, err := db.ListChildOrders(demand.ID)
	testutil.MustNoErr(t, err, "list the dig's legs")
	if len(legs) == 0 {
		t.Fatal("fixture: the dig has no legs")
	}
	for _, l := range legs {
		if !protocol.IsTerminal(l.Status) {
			t.Errorf("leg %d is %s after its parent's cancel, want terminal — the cascade tears the "+
				"dig down as it did before the soft hold", l.ID, l.Status)
		}
	}
}

// TestC2bPin_APlainBuriedRetrievesDigKeepsItsLineClosed pins the arm A.1 added
// to orders.InFlightForDropoffSQL, which was caught by reading and not by a
// test. A plain buried retrieve's last leg delivers to the parent's line. Its
// legs are born holding nothing now, so while the dig runs the leg bound for the
// line holds no claim; it still counts as inbound there, and a second order must
// not pass the line's dropoff gate in that window.
//
// MUTATION: drop the dig-leg arm from InFlightForDropoffSQL; the gate opens and
// this fires.
func TestC2bPin_APlainBuriedRetrievesDigKeepsItsLineClosed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)
	line := lineNode(t, db, "LINE-A1BD")
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-A1BD-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-A1BD-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	demand := mkDigOrder(t, db, "a1bd-demand", bp.Code, line.Name)
	if _, pe := d.planner.planBuriedReshuffle(demand,
		&BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID}); pe != nil {
		t.Fatalf("fixture: the plain dig did not plan: %s: %s", pe.Code, pe.Detail)
	}
	legs, err := db.ListChildOrders(demand.ID)
	testutil.MustNoErr(t, err, "list the dig's legs")
	toLine := 0
	for _, l := range legs {
		if l.DeliveryNode == line.Name {
			toLine++
		}
	}
	if toLine == 0 {
		t.Fatalf("fixture: no leg of the plain dig delivers to %s", line.Name)
	}

	second := mkDigOrder(t, db, "a1bd-second", bp.Code, line.Name)
	if blocked, _ := CheckDropoffCapacity(db, line.Name, second.ID); !blocked {
		t.Fatalf("a second order passed %s's dropoff gate while demand %d's dig runs, with a leg "+
			"bound for that line not yet sent — two bins delivered to one line position", line.Name, demand.ID)
	}
}

func pendingBinRows(t *testing.T, db *store.DB, orderID, binID int64) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM reservations
		WHERE order_id = $1 AND resource_kind = 'bin' AND bin_id = $2 AND state = 'pending'`,
		orderID, binID).Scan(&n), "count pending bin rows")
	return n
}
