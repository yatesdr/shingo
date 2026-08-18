//go:build docker

package bins_test

import (
	"shingocore/store/reservations"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// empty_order_pins_test.go — MG3-0, pin 7. The ORDERING every empty finder
// rests on, asserted across ALL FOUR of them.
//
// WHY ALL FOUR AND NOT ONE. `AccessibleEmptyOrder` is appended by each finder
// separately. Phase 3 consolidates their WHERE bodies into a shared fragment
// family, and the ordering is the part of each query NOT being consolidated —
// so it is exactly the part that can silently stop being applied to one of
// them. A consolidation that dropped the ORDER BY from a single finder would
// leave that finder picking by bin id, which is the lane-blind FIFO the
// 2026-06-13 rewrite existed to delete: the planner picks a buried empty and
// then reshuffles the bins on top of it, every time, instead of taking the one
// standing at the mouth.
//
// THE FIXTURE IS ONE LANE, DELIBERATELY. Accessibility, depth and id all
// disagree inside it, so each rung of the ladder is load-bearing: the mouth bin
// has the HIGHEST id, so an id-only ordering picks the buried one and fails.

// laneWithBins builds a group → lane → three depth-ordered slots, and lands one
// empty carrier of `code` at each. Returns the group id and the bin ids in
// depth order (mouth first).
func laneWithBins(t *testing.T, db *store.DB, prefix, code string) (int64, []int64) {
	t.Helper()
	sdb := db.DB

	grpID, err := nodes.CreateGroup(sdb, prefix+"-GRP")
	testutil.MustNoErr(t, err, "CreateGroup")

	laneType, err := nodes.GetTypeByCode(sdb, "LANE")
	testutil.MustNoErr(t, err, "LANE node type")
	lane := &nodes.Node{Name: prefix + "-LANE", Enabled: true, ParentID: &grpID,
		IsSynthetic: true, NodeTypeID: &laneType.ID}
	testutil.MustNoErr(t, nodes.Create(sdb, lane), "create lane")

	var btID int64
	testutil.MustNoErr(t, sdb.QueryRow(
		`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`, code).Scan(&btID), "bin type")

	// DESCENDING ids down the lane: the MOUTH gets the highest id, so ordering
	// by id alone picks the deepest bin and the pin fails. Built by inserting
	// the deep slots' carriers first.
	var ids []int64
	depths := []int{1, 2, 3}
	slotIDs := make([]int64, len(depths))
	for i, d := range depths {
		depth := d
		slot := &nodes.Node{Name: prefix + "-SLOT-" + string(rune('0'+d)), Enabled: true,
			ParentID: &lane.ID, Depth: &depth}
		testutil.MustNoErr(t, nodes.Create(sdb, slot), "create slot")
		slotIDs[i] = slot.ID
	}
	for i := len(depths) - 1; i >= 0; i-- {
		var id int64
		testutil.MustNoErr(t, sdb.QueryRow(
			`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,$2,$3,'available') RETURNING id`,
			btID, prefix+"-BIN-D"+string(rune('0'+depths[i])), slotIDs[i]).Scan(&id), "carrier")
		ids = append([]int64{id}, ids...)
	}
	return grpID, ids
}

// THE MOUTH WINS, in every one of the four finders.
func TestPin_EveryEmptyFinderPrefersTheMouth(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	grpID, ids := laneWithBins(t, db, "ORD", "ORD-45x58")
	mouth := ids[0]

	if !(ids[0] > ids[1] && ids[1] > ids[2]) {
		t.Fatalf("fixture: bin ids %v are not descending down the lane, so an id-only "+
			"ordering would pass this test for the wrong reason", ids)
	}

	for _, tc := range []struct {
		name string
		find func() (*bins.Bin, error)
	}{
		{"typed, group-scoped", func() (*bins.Bin, error) {
			return bins.FindEmptyOfTypeInGroup(db.DB, "ORD-45x58", grpID, 0, reservations.Anyone)
		}},
		{"untyped, group-scoped", func() (*bins.Bin, error) {
			return bins.FindEmptyCompatibleInGroup(db.DB, "", grpID, 0, reservations.Anyone)
		}},
		{"typed, plant-wide", func() (*bins.Bin, error) {
			return bins.FindEmptyOfType(db.DB, "ORD-45x58", "", 0, bins.EmptyFence{}, reservations.Anyone)
		}},
		{"untyped, plant-wide", func() (*bins.Bin, error) {
			return bins.FindEmptyCompatible(db.DB, "", "", 0, bins.EmptyFence{}, reservations.Anyone)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.find()
			testutil.MustNoErr(t, err, "find")
			if got == nil {
				t.Fatal("found nothing — the fixture does not reach this finder")
			}
			if got.ID != mouth {
				t.Errorf("picked bin %d, want the lane mouth %d. Ordering by id alone picks "+
					"the DEEPEST carrier here, and then tier 6 digs out everything on top of "+
					"it — the lane-blind FIFO the 2026-06-13 rewrite deleted", got.ID, mouth)
			}
		})
	}
}

// THE DEPTH/ID TIEBREAK, when accessibility cannot separate the candidates.
//
// Two carriers EQUALLY buried — same lane, both behind an occupied mouth — are
// separated by depth first and only then by id. Without the depth rung the
// finder would pick the deeper one whenever it happened to have the lower id,
// and tier 6 would then dig two slots instead of one.
func TestPin_EquallyBuriedBreaksOnDepthThenID(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	grpID, ids := laneWithBins(t, db, "TIE", "TIE-45x58")

	// Occupy the mouth with something UNSOURCEABLE, so both remaining carriers
	// are buried and accessibility ranks them equal.
	_, err := db.Exec(`UPDATE bins SET locked = true WHERE id = $1`, ids[0])
	testutil.MustNoErr(t, err, "lock the mouth")

	got, err := bins.FindEmptyOfTypeInGroup(db.DB, "TIE-45x58", grpID, 0, reservations.Anyone)
	testutil.MustNoErr(t, err, "find")
	if got == nil {
		t.Fatal("found nothing behind a locked mouth")
	}
	if got.ID != ids[1] {
		t.Errorf("picked bin %d, want the shallower buried carrier %d (depth 2 beats depth 3). "+
			"The two are equally inaccessible, so depth is the rung that decides — and it "+
			"decides how many bins tier 6 has to move", got.ID, ids[1])
	}
}
