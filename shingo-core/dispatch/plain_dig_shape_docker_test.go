//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// plain_dig_shape_docker_test.go — CHARACTERIZATION, written before the A batch
// changes anything, so that "the plain path is untouched" is a measurement
// rather than an intention.
//
// ── WHY THIS PIN AND NOT ANOTHER ──────────────────────────────────────────
//
// The A batch turns COMPLEX burials into service digs: the demand stops being
// re-parented, stays `queued` with its cause, and something else does the
// digging. The ruling carves out exactly one exception — a PLAIN retrieve, where
// the dig's last leg IS the demand's whole job, so re-parenting the demand costs
// nothing and saves a hand-off (round-1 synthesis §1.3, plan §12.40).
//
// That carve-out is the whole reason the two paths must diverge, and it was
// asserted nowhere by name. `TestPlanReshuffle_*` pin the PLANNER's geometry and
// `TestPlanBuriedReshuffle_*` pin its dispositions; none of them says "and the
// demand becomes its own dig's parent", which is the property the batch is about
// to remove from the other path. A refactor that accidentally converted plain to
// the service shape too would pass every existing test in this package.
//
// So: three assertions, all about SHAPE rather than geometry, and all of them
// things the complex path will stop doing.
func TestPlainBuriedRetrieve_KeepsDemandAsItsOwnDigParent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	blocker := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-PLAIN-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-PLAIN-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	demand := mkDigOrder(t, db, "plain-dig-shape", bp.Code, "LINE-PLAIN")

	if _, pe := d.planner.planBuriedReshuffle(demand,
		&BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID}); pe != nil {
		t.Fatalf("fixture: the plain dig did not plan: %s: %s", pe.Code, pe.Detail)
	}

	// (1) THE DEMAND IS THE PARENT. Every leg hangs off the retrieve that
	// discovered the burial — there is no separate service order, and the demand
	// did not stay at the top of the tree with a dig beside it.
	kids, err := db.ListChildOrders(demand.ID)
	testutil.MustNoErr(t, err, "list the dig's legs")
	if len(kids) == 0 {
		t.Fatal("the plain dig created no legs")
	}
	for _, k := range kids {
		if k.ParentOrderID == nil || *k.ParentOrderID != demand.ID {
			t.Errorf("leg %d hangs off %v, want the demand %d — the plain path re-parents the "+
				"demand, and that is the carve-out the A batch preserves", k.ID, k.ParentOrderID, demand.ID)
		}
	}

	// (2) THE DEMAND MOVED TO `reshuffling`. This is the status excursion the
	// complex path is losing; here it is correct, because the demand really is
	// being re-planned and no vehicle is committed to it yet.
	got, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "reload the demand")
	if got.Status != protocol.StatusReshuffling {
		t.Errorf("demand status = %q, want %q — the plain demand becomes its own dig and says so",
			got.Status, protocol.StatusReshuffling)
	}

	// (3) THE DIG CARRIES THE DEMAND'S OWN JOB. PlanReshuffle ends by retrieving
	// the target; PlanReshuffleUnburyOnly (the complex planner) deliberately emits
	// no retrieve step at all. A leg that moves the TARGET bin is therefore the
	// difference between the two planners, and it is why the plain demand needs no
	// resumption: when the last leg lands, the work is done.
	movesTarget := false
	for _, k := range kids {
		if k.BinID != nil && *k.BinID == target.ID {
			movesTarget = true
		}
	}
	if !movesTarget {
		t.Errorf("no leg moves the target bin %d — the plain dig is supposed to CARRY the demand's "+
			"job (PlanReshuffle), not merely expose the bin and hand back (PlanReshuffleUnburyOnly). "+
			"If this fails after the A batch, the plain path was converted to the service shape too, "+
			"which the ruling's one carve-out forbids", target.ID)
	}
	_ = blocker

	// And the lane is held for the excavation, same as any dig.
	if !d.laneLock.IsLocked(lane.ID) {
		t.Error("the plain dig created legs without holding the lane")
	}
}

// serviceDigFor finds the lane-clear dig raised on a demand's behalf.
//
// ── THE LOOKUP KEY IS ITSELF THE RULING ───────────────────────────────────
//
// There is no requester column and there will not be one (PLAN §R.40): a dig is
// a service to a LANE and one dig frees every demand waiting behind the same
// wall, so a 1:1 pointer would assert something false and would go stale the
// moment that one requester cancelled. What ties a dig to the work that caused
// it is the EPISODE — the origin it inherits — which is also what puts the cost
// of digging in the right place.
//
// So a test cannot follow a pointer, because deliberately there is none. It
// identifies the dig by its SHAPE instead: a top-level Move order that is not the
// demand and that owns legs. Inside one test's own database that is unambiguous —
// testdb hands every test a private clone, and a fixture raises one burial. It
// would not be a safe query against a plant, which is the point: the production
// relationship is the episode, and a test asserting on episode identity is
// covered by the origin-propagation tests rather than by this helper.
func serviceDigFor(t *testing.T, db *store.DB, demand *orders.Order) *orders.Order {
	t.Helper()
	rows, err := db.DB.Query(
		`SELECT id FROM orders WHERE id <> $1 AND parent_order_id IS NULL AND order_type = $2 ORDER BY id DESC`,
		demand.ID, OrderTypeMove)
	if err != nil {
		t.Fatalf("look for the service dig: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan candidate dig: %v", err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		kids, err := db.ListChildOrders(id)
		if err != nil {
			t.Fatalf("list children of candidate dig %d: %v", id, err)
		}
		if len(kids) == 0 {
			continue
		}
		dig, err := db.GetOrder(id)
		if err != nil {
			t.Fatalf("read service dig %d: %v", id, err)
		}
		return dig
	}
	t.Fatalf("no service dig was raised for demand %d — the demand stays queued and something else "+
		"digs, so a burial with no dig means the excavation was never proposed", demand.ID)
	return nil
}

// serviceDigChildren is the legs of the dig raised for this demand — what used to
// be ListChildOrders(demand.ID) before the demand stopped being its own dig.
func serviceDigChildren(t *testing.T, db *store.DB, demand *orders.Order) []*orders.Order {
	t.Helper()
	dig := serviceDigFor(t, db, demand)
	kids, err := db.ListChildOrders(dig.ID)
	testutil.MustNoErr(t, err, "list the service dig's legs")
	return kids
}
