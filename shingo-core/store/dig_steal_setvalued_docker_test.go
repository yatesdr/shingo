//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// dig_steal_setvalued_docker_test.go — the steal repairs EVERY holder, and it
// never repairs a person's order by breaking it.
//
// The steal read one victim with an unordered QueryRow and cleared one pointer.
// That was exactly right while uq_reservations_bin_active made one active row
// per bin structural, and it is a coin flip the moment more than one row can
// exist: N-1 holders keep a bin_id pointing at a bin the dig has taken, their
// reservations are deleted by supersedeBinLedger a statement later, and every
// one of them re-enters through dispatchHeldBin — which never re-acquires — and
// wedges on claim-failed forever.
//
// The other half is who may be repaired that way at all. An order whose caller
// is a PERSON at a Core door is never silently re-aimed: un-pointing it hands it
// back to the finder, which is Core choosing a different bin than the one
// somebody named. Those keep their pointer and end loudly.

// dropBinUniqueIndex removes uq_reservations_bin_active for the life of one
// test's database.
//
// THIS IS THE PIN THAT GOES LIVE AT THE NARROWING, and it is written this way
// deliberately rather than skipped. Under today's index a bin cannot carry two
// active rows, so the multi-victim path is unreachable through the ordinary
// primitives and the set-valued code would ship untested — which is how the
// one-victim read came to be correct-by-accident in the first place. The
// narrowing is the change that makes this arrangement ordinary; until then the
// test manufactures the state the narrowing will produce.
//
// testdb.Open gives each test its own database, so dropping an index here
// reaches nothing else.
func dropBinUniqueIndex(t *testing.T, db *store.DB) {
	t.Helper()
	if _, err := db.DB.Exec(`DROP INDEX IF EXISTS uq_reservations_bin_active`); err != nil {
		t.Fatalf("drop uq_reservations_bin_active: %v", err)
	}
}

// reserveBinNoIndex writes an active bin reservation directly, for the arranged
// state above. Not reservations.Acquire: Acquire's whole contract is the index
// this arrangement has removed, so going through it would assert nothing.
func reserveBinNoIndex(t *testing.T, db *store.DB, orderID, binID int64, state string) {
	t.Helper()
	if _, err := db.DB.Exec(
		`INSERT INTO reservations (order_id, resource_kind, bin_id, state, reserved_by)
		 VALUES ($1, 'bin', $2, $3, 'test')`, orderID, binID, state); err != nil {
		t.Fatalf("insert reservation for order %d: %v", orderID, err)
	}
}

// TestDigSteal_RepairsEveryHolderNotOne is the set-valued contract.
//
// MUTATION (verified): put the victim scan back on QueryRow. Two of the three
// holders keep a bin_id pointing at a bin the dig owns, with no reservation
// behind it — the claim-failed-forever shape, and the exact state contract-v2
// clause (iii) exists to detect.
func TestDigSteal_RepairsEveryHolderNotOne(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "STEAL-MANY")
	dropBinUniqueIndex(t, db)

	holders := make([]*orders.Order, 3)
	for i := range holders {
		h := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = "steal-many-holder-" + string(rune('a'+i))
		})
		reserveBinNoIndex(t, db, h.ID, bin.ID, "pending")
		testutil.MustNoErr(t, db.UpdateOrderBinID(h.ID, bin.ID), "stamp holder bin_id")
		holders[i] = h
	}

	parent := compoundParent(t, db, "steal-many-parent")
	displaced, err := db.CreateCompoundChildren([]store.CompoundChild{childFor(parent, bin.ID, 1)})
	testutil.MustNoErr(t, err, "the dig must win a soft-held blocker")
	if len(displaced) != 0 {
		t.Errorf("displaced = %+v, want none — none of these holders is hand-placed", displaced)
	}

	for _, h := range holders {
		after, err := db.GetOrder(h.ID)
		testutil.MustNoErr(t, err, "reload holder")
		if after.BinID != nil {
			t.Errorf("holder %d still points at bin %d. Its reservation is gone (the ledger is "+
				"swept whole) and dispatchHeldBin never re-acquires, so this order confirms by id "+
				"against a row that does not exist, every tick, forever", h.ID, *after.BinID)
		}
		if protocol.IsTerminal(after.Status) {
			t.Errorf("holder %d is %q — losing a reserved bin is a recalculation, not a failure",
				h.ID, after.Status)
		}
	}

	// The ledger is swept once and re-written once, whatever the victim count.
	rows := activeBinReservations(t, db, bin.ID)
	if len(rows) != 1 {
		t.Fatalf("bin has %d active reservation(s), want exactly 1 (the dig's) — three victims must "+
			"not leave three rows", len(rows))
	}
}

// TestDigSteal_LeavesAHandPlacedOrderPointedAndReportsIt is the demand-by-hand
// half.
//
// A bin move is a person saying "put THAT bin over there". Un-pointing it sends
// it back to the finder, which for a node-local source means Core picks whatever
// bin is standing at that node now — a different bin, moved on somebody's
// instruction, with nothing anywhere saying the instruction was changed. The
// order keeps its pointer here and its disposition belongs to the dispatch
// caller, which fails it loudly with a code of its own.
//
// AND IT MUST NEVER REACH THE STRUCTURAL ARM. That is what happened before: the
// door stamped no source intent, so the scanner's own move exemption could not
// see the order and killed it with "order has empty payload_code" — a
// construction-bug sentence for an order nobody constructed wrongly.
func TestDigSteal_LeavesAHandPlacedOrderPointedAndReportsIt(t *testing.T) {
	t.Parallel()
	// The store's half of the answer is deliberately half: it leaves the order
	// pointed and REPORTS it, and the disposition belongs to the dispatch caller,
	// which is not in this test. So the row this test ends on IS the wedge, on
	// purpose — dig_hand_placed_docker_test.go is where it gets ended.
	testdb.DisableWedgeSweep(t, "the store reports the displaced order; the dispatch caller ends it, and is not here")
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "STEAL-HAND")

	byHand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "steal-hand-move"
		o.OrderType = protocol.OrderTypeMove
		o.OriginClass = protocol.OriginClassNoDemand
		o.SourceNode = sd.StorageNode.Name
	})
	testdb.ReserveBin(t, db, byHand.ID, bin.ID)
	testutil.MustNoErr(t, db.UpdateOrderBinID(byHand.ID, bin.ID), "stamp bin_id")

	parent := compoundParent(t, db, "steal-hand-parent")
	child := childFor(parent, bin.ID, 1)
	child.Order.DeliveryNode = sd.StorageNode.Name
	displaced, err := db.CreateCompoundChildren([]store.CompoundChild{child})
	testutil.MustNoErr(t, err, "the dig still wins")

	if len(displaced) != 1 || displaced[0].OrderID != byHand.ID {
		t.Fatalf("displaced = %+v, want exactly order %d — a hand-placed order the dig took a bin "+
			"from has to be REPORTED, or its disposition is nobody's job", displaced, byHand.ID)
	}
	if displaced[0].BinID != bin.ID {
		t.Errorf("displaced bin = %d, want %d", displaced[0].BinID, bin.ID)
	}

	after, err := db.GetOrder(byHand.ID)
	testutil.MustNoErr(t, err, "reload hand-placed order")
	if after.BinID == nil || *after.BinID != bin.ID {
		t.Errorf("the hand-placed order was un-pointed. That hands it to the finder, and a "+
			"node-local move re-sourced from its node takes whatever bin is standing there — "+
			"Core silently moving a different bin than the one a person named (bin_id = %v)",
			after.BinID)
	}
}
