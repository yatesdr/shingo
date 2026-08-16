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

// serviceDigFor finds the lane-clear dig raised on a demand's behalf, which is
// USUALLY THE DEMAND ITSELF now.
//
// ── THE LOOKUP KEY WAS THE RULING, AND THE RULING CHANGED ─────────────────
//
// It used to say: "There is no requester column and there will not be one (PLAN
// §R.40): a dig is a service to a LANE and one dig frees every demand waiting
// behind the same wall, so a 1:1 pointer would assert something false and would
// go stale the moment that one requester cancelled. What ties a dig to the work
// that caused it is the EPISODE... So a test cannot follow a pointer, because
// deliberately there is none. It identifies the dig by its SHAPE instead: a
// top-level Move order that is not the demand and that owns legs."
//
// §R.91 gives most digs an owner: the demand that caused it re-parents onto it.
// So the first question is no longer "which other order is the dig" but "is this
// demand its own dig", and the answer is its child rows.
//
// THE FOLDER SEARCH IS KEPT, not as a fallback but as the other half of a
// two-shape world: the gate-dweller heal still mints a folder, on physics, and
// its dig genuinely is a different order with no pointer back. Everything the
// quoted paragraph says about episodes and 1:many is still true of THAT shape,
// and the shape-based search below is still how a test finds it.
func serviceDigFor(t *testing.T, db *store.DB, demand *orders.Order) *orders.Order {
	t.Helper()

	// THE DEMAND IS ITS OWN DIG (§R.91) — the ordinary case. Asked first, because
	// under the re-parent there is frequently no other order at all to find, and
	// falling through to the folder search would report "no dig was raised" about
	// a demand that is at that moment excavating.
	if kids, err := db.ListChildOrders(demand.ID); err == nil && len(kids) > 0 {
		return demand
	}
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
	t.Fatalf("no dig was raised for demand %d — it owns no legs of its own and no folder dig exists "+
		"either, so the excavation was never proposed", demand.ID)
	return nil
}

// serviceDigChildren is the legs of the dig raised for this demand. Its own
// header used to read "what used to be ListChildOrders(demand.ID) before the
// demand stopped being its own dig" — under §R.91 that is what it is again for
// every shape but the gate-dweller folder, and serviceDigFor is what tells them
// apart.
func serviceDigChildren(t *testing.T, db *store.DB, demand *orders.Order) []*orders.Order {
	t.Helper()
	dig := serviceDigFor(t, db, demand)
	kids, err := db.ListChildOrders(dig.ID)
	testutil.MustNoErr(t, err, "list the service dig's legs")
	return kids
}
