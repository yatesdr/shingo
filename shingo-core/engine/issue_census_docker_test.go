//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// issue_census_docker_test.go — the census's docker arm for the doors the
// engine opens (testdb.IssueDoors, Arm "engine"; dispatch's arm opens the
// rest). Each door issues one order on a database of its own with the
// at-commit record installed; what the order held as its issue committed is
// held to its census row, whatever a scanner pass does after, and a door that
// names a cart is issued again with a stranger's pending hold on it
// (testdb.RunIssueCensus).

func censusOrderByUUID(t *testing.T, db *store.DB, uuid string) int64 {
	t.Helper()
	o, err := db.GetOrderByUUID(uuid)
	if err != nil || o == nil {
		return 0
	}
	return o.ID
}

// censusRecovery is the carried-bin recovery's world: a part's bin on a
// robot's deck whose claim sources the part from a store group with one free
// slot, and the robot at rest.
func censusRecovery(t *testing.T, stranger bool) (*store.DB, *Engine, *orders.Order, string) {
	t.Helper()
	db := testdb.IssueCensusDB(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	grp, _ := storeGroupWithSlot(t, db, "ICRB")
	seedClaim(t, db, "PROC-ICRB", "STY", "LINE-ICRB", "ICRB-P", grp.Name, true)
	bin, carrier := seedCancelledCarry(t, db, "AMR-IC", "ICRB-P", "LINE-ICRB")
	if stranger {
		testdb.StrangerHolds(t, db, bin.ID)
	}
	cacheRobot(eng, dispatchableRobot("AMR-IC"))
	return db, eng, carrier, recoveryEdgeUUID(bin.ID, "AMR-IC")
}

// censusClear taps a dedicated loader's home clear (openSpot: a free buffer
// spot, else the trade) with stranger's hold on the bin cart picks, and
// returns the clear's parent, or nil when the tap was refused.
func censusClear(t *testing.T, prefix string, openSpot bool, stranger func(*clearRig) int64) (*clearRig, *orders.Order) {
	t.Helper()
	r := newClearRig(t, prefix, openSpot)
	testdb.InstallIssueCensus(t, r.db)
	if stranger != nil {
		testdb.StrangerHolds(t, r.db, stranger(r))
	}
	r.tap(t)
	return r, r.parent(t)
}

func clearPartial(r *clearRig) int64 { return r.p.ID }
func clearEmpty(r *clearRig) int64   { return r.e.ID }

// strangerOn is the cart pick when the world has a stranger, nil otherwise.
func strangerOn(stranger bool, cart func(*clearRig) int64) func(*clearRig) int64 {
	if stranger {
		return cart
	}
	return nil
}

var engineIssueDrivers = map[string]testdb.IssueDriver{
	"person's bin move": func(t *testing.T, stranger bool) (*store.DB, int64) {
		db := testdb.IssueCensusDB(t)
		storage, line, bp := setupTestData(t, db)
		cart := createTestBinAtNode(t, db, bp.Code, storage.ID, "IC-MOVE")
		if stranger {
			testdb.StrangerHolds(t, db, cart.ID)
		}
		eng := newTestEngine(t, db, simulator.New())
		res, err := eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionByLabel, BinLabel: cart.Label,
			DestNodeID: line.ID, StationID: "ic"})
		if err != nil {
			t.Logf("the move was refused: %v", err)
			return db, 0
		}
		return db, res.OrderID
	},
	"stage 2's pull": func(t *testing.T, stranger bool) (*store.DB, int64) {
		db := testdb.IssueCensusDB(t)
		f := newS2Fixture(t, db, "ICS", 1)
		p := f.pair(t, "ICS", 1)
		cart := f.cart(t, f.slots[0].ID, f.marker, time.Hour)
		if stranger {
			testdb.StrangerHolds(t, db, cart.ID)
		}
		eng := newTestEngine(t, db, simulator.New())
		_, err := eng.dispatcher.PullStage2(p.stage1)
		testutil.MustNoErr(t, err, "pull")
		if moves := s2Moves(t, db); len(moves) == 1 {
			return db, moves[0].ID
		}
		return db, 0
	},
	"carried-bin recovery": func(t *testing.T, stranger bool) (*store.DB, int64) {
		db, eng, carrier, uuid := censusRecovery(t, stranger)
		if _, _, err := eng.RecoverCarriedBin(*carrier.BinID, "operator:test"); err != nil {
			t.Logf("the recovery refused: %v", err)
		}
		return db, censusOrderByUUID(t, db, uuid)
	},
	"the cancel's return": func(t *testing.T, stranger bool) (*store.DB, int64) {
		db, eng, carrier, uuid := censusRecovery(t, stranger)
		bin, err := db.GetBin(*carrier.BinID)
		testutil.MustNoErr(t, err, "the carried bin")
		eng.tryReturnCarriedBin(bin, "AMR-IC", loadedDispatchable("AMR-IC"), carrier)
		return db, censusOrderByUUID(t, db, uuid)
	},
	"home clear": func(t *testing.T, _ bool) (*store.DB, int64) {
		r, parent := censusClear(t, "ICH", true, nil)
		if parent == nil {
			return r.db, 0
		}
		return r.db, parent.ID
	},
	"home clear's child": func(t *testing.T, stranger bool) (*store.DB, int64) {
		r, parent := censusClear(t, "ICC", true, strangerOn(stranger, clearPartial))
		if parent == nil {
			return r.db, 0
		}
		if partial, _ := r.children(t, parent.ID); partial != nil {
			return r.db, partial.ID
		}
		return r.db, 0
	},
	"home clear's spot move": func(t *testing.T, stranger bool) (*store.DB, int64) {
		r, parent := censusClear(t, "ICP", true, strangerOn(stranger, clearEmpty))
		if parent == nil {
			return r.db, 0
		}
		if _, empties := r.children(t, parent.ID); len(empties) == 1 {
			return r.db, empties[0].ID
		}
		return r.db, 0
	},
	"home clear's trade": func(t *testing.T, stranger bool) (*store.DB, int64) {
		r, parent := censusClear(t, "ICT", false, strangerOn(stranger, clearEmpty))
		if parent == nil {
			return r.db, 0
		}
		if _, empties := r.children(t, parent.ID); len(empties) == 1 {
			return r.db, empties[0].ID
		}
		return r.db, 0
	},
	"maintained-group level keeper": func(t *testing.T, _ bool) (*store.DB, int64) {
		db := testdb.IssueCensusDB(t)
		eng := newTestEngine(t, db, simulator.New())
		_, btID := mntFixture(t, db, "ICK", 1, "ICK-45x58", 1)
		mntOutsideEmpty(t, db, btID, "ICK-MARKET")
		eng.Maintainer().Tick()
		var id int64
		testutil.MustNoErr(t, db.QueryRow(`SELECT id FROM orders WHERE order_type = 'retrieve_empty'
			ORDER BY id DESC LIMIT 1`).Scan(&id), "the keeper's ask")
		return db, id
	},
}

// TestIssueCensus_AtCommit issues one order through each door the engine
// opens and holds what it held at commit to its census row.
func TestIssueCensus_AtCommit(t *testing.T) {
	t.Parallel()
	testdb.RunIssueCensus(t, "engine", engineIssueDrivers)
}
