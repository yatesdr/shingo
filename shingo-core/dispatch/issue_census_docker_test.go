//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// issue_census_docker_test.go — the census's docker arm for the doors this
// package opens (testdb.IssueDoors, Arm "dispatch"; engine's arm opens the
// rest). Each door issues one order on a database of its own with the
// at-commit record installed and no acquirer wired, and what the order held
// as its issue committed is held to its census row; a door that names a cart
// is issued again with a stranger's pending hold on it (testdb.RunIssueCensus).

func orderIDByUUID(t *testing.T, db *store.DB, uuid string) int64 {
	t.Helper()
	o, err := db.GetOrderByUUID(uuid)
	if err != nil || o == nil {
		return 0
	}
	return o.ID
}

var dispatchIssueDrivers = map[string]testdb.IssueDriver{
	"wire plain order": func(t *testing.T, _ bool) (*store.DB, int64) {
		db := testdb.IssueCensusDB(t)
		_, line, bp := setupTestData(t, db)
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
		d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: "ic-plain",
			OrderType: OrderTypeRetrieve, PayloadCode: bp.Code, Quantity: 1, DeliveryNode: line.Name})
		return db, orderIDByUUID(t, db, "ic-plain")
	},
	"wire named move": func(t *testing.T, stranger bool) (*store.DB, int64) {
		db := testdb.IssueCensusDB(t)
		storage, _, bp := setupTestData(t, db)
		// The drop is a storage node, one carrier's at a time, so the move
		// holds it from its issue (a plain drop holds nothing:
		// TestNamedBinMove_HoldsAnExclusiveDropFromItsIssue).
		drop := &nodes.Node{Name: "IC-NAMED-DROP", Enabled: true, NodeTypeID: storage.NodeTypeID}
		testutil.MustNoErr(t, db.CreateNode(drop), "the move's drop")
		cart := testdb.CreateBinAtNode(t, db, bp.Code, storage.ID, "IC-NAMED")
		if stranger {
			testdb.StrangerHolds(t, db, cart.ID)
		}
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
		d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: "ic-named",
			OrderType: OrderTypeMove, PayloadCode: bp.Code, Quantity: 1, BinID: cart.ID,
			SourceNode: storage.Name, DeliveryNode: drop.Name})
		return db, orderIDByUUID(t, db, "ic-named")
	},
	"complex intake": func(t *testing.T, _ bool) (*store.DB, int64) {
		db := testdb.IssueCensusDB(t)
		storage, line, bp := setupTestData(t, db)
		testdb.CreateBinAtNode(t, db, bp.Code, storage.ID, "IC-CPX")
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
		d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{OrderUUID: "ic-complex",
			PayloadCode: bp.Code, Steps: []protocol.ComplexOrderStep{
				{Action: protocol.ActionPickup, Node: storage.Name},
				{Action: protocol.ActionDropoff, Node: line.Name},
			}})
		return db, orderIDByUUID(t, db, "ic-complex")
	},
	// The leg the row describes is the first: the blocker's unbury.
	"dig legs": func(t *testing.T, stranger bool) (*store.DB, int64) {
		db := testdb.IssueCensusDB(t)
		grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)
		parent := &orders.Order{EdgeUUID: "ic-dig-parent", StationID: "line-1",
			OrderType: OrderTypeComplex, Status: StatusQueued, OriginClass: protocol.OriginClassNoDemand}
		testutil.MustNoErr(t, db.CreateOrder(parent), "the dig's parent")
		blocker := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "IC-DIG-BLK")
		createTestBinAtNode(t, db, bp.Code, slots[1].ID, "IC-DIG-TGT")
		if stranger {
			testdb.StrangerHolds(t, db, blocker.ID)
		}
		plan, err := PlanLaneMouthClear(db, slots[1], lane, grp.ID, reservations.Anyone)
		testutil.MustNoErr(t, err, "plan the unbury")
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
		if err := d.CreateCompoundOrder(parent, plan); err != nil {
			t.Logf("the dig was refused: %v", err)
			return db, 0
		}
		children, err := db.ListChildOrders(parent.ID)
		testutil.MustNoErr(t, err, "the dig's legs")
		for _, c := range children {
			if c.Sequence == 1 {
				return db, c.ID
			}
		}
		return db, 0
	},
	"loader drain": func(t *testing.T, stranger bool) (*store.DB, int64) {
		db := testdb.IssueCensusDB(t)
		f := newDrainFixture(t, db, "ICD", 1, 1, protocol.CarrierLeftover)
		cart := f.cart(t, f.slots[0].ID, f.tote, totes(13))
		if stranger {
			testdb.StrangerHolds(t, db, cart.ID)
		}
		_, err := f.d.DrainLoader(f.loader)
		testutil.MustNoErr(t, err, "drain")
		if moves := drainMoves(t, db); len(moves) == 1 {
			return db, moves[0].ID
		}
		return db, 0
	},
	"loader replenish": func(t *testing.T, _ bool) (*store.DB, int64) {
		db := testdb.IssueCensusDB(t)
		req, cfg, _ := replenishFixture(t, db, "ICR", 1)
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
		res, err := d.ReplenishLoader(req, cfg)
		testutil.MustNoErr(t, err, "replenish")
		if len(res.Created) != 1 {
			t.Fatalf("the replenish asked %d carriers, want 1 (held: %v, skipped: %q)", len(res.Created), res.HeldBy, res.Skipped)
		}
		return db, res.Created[0].ID
	},
}

// TestIssueCensus_AtCommit issues one order through each door this package
// opens and holds what it held at commit to its census row.
func TestIssueCensus_AtCommit(t *testing.T) {
	t.Parallel()
	testdb.RunIssueCensus(t, "dispatch", dispatchIssueDrivers)
}

// TestIssueCensus_ADrainMoveOffAnOnDeckDoesNotHoldThePair: a pair at a press
// waits for outside material while a move naming its bin lifts the on-deck's
// cart (allocator's vacant pickup, LiveNamedMoveOff, waiting_for_outside). A
// drain's move off that position is Core's errand clearing a cart, not that
// move, and must not hold the pair. It does not today because the drain's row
// names no bin; the day every order for one carrier writes named_bin_id, this
// is the pin that the hold's cause still means a station's move.
func TestIssueCensus_ADrainMoveOffAnOnDeckDoesNotHoldThePair(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := newDrainFixture(t, db, "ICO", 1, 1, protocol.CarrierLeftover)
	f.cart(t, f.slots[0].ID, f.tote, totes(13))
	_, err := f.d.DrainLoader(f.loader)
	testutil.MustNoErr(t, err, "drain")
	if moves := drainMoves(t, db); len(moves) != 1 || moves[0].SourceNode != f.slots[0].Name {
		t.Fatalf("drain moves = %d, want one off %s", len(moves), f.slots[0].Name)
	}
	pair := testdb.CreateOrder(t, db)
	lifting, err := db.LiveNamedMoveOff(f.slots[0].Name, pair.ID)
	testutil.MustNoErr(t, err, "the pair's hold")
	if lifting {
		t.Errorf("a drain's move off %s holds a pair there as a station's named move would", f.slots[0].Name)
	}
}
