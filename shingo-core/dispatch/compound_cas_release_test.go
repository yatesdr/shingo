//go:build docker

package dispatch

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/reservations"
)

// stealChildAtOccupancyInsert makes the CAS loss DETERMINISTIC instead of racing
// for it.
//
// The window this exercises is real but narrow: two callers resolve to the same
// child (GetNextChildOrder selects `status='pending' … LIMIT 1`), both clear
// admission, one inserts the occupancy row, and then exactly one of their
// compare-and-swaps matches a row. Reproducing that with goroutines gives a test
// that passes identically whether or not the interleaving ever occurred — the
// vacuous shape compound_concurrency_test.go's header warns about.
//
// So the winner is played by a trigger that fires ON the occupancy insert, which
// is precisely where a winning caller's status write lands: after this caller
// took the row (dispatch.TakeLaneOccupancy) and before it CASes
// (LifecycleService.MoveToSourcing). The trigger moves the child pending →
// sourcing, so the CAS finds no `pending` row and returns ConcurrentTransition.
// That is the loser's arm, entered on demand.
//
// Scoped to occupancy inserts so setup reservations are untouched.
func stealChildAtOccupancyInsert(t *testing.T, db *store.DB) {
	t.Helper()
	stmts := []string{
		`CREATE OR REPLACE FUNCTION test_steal_child() RETURNS trigger AS $$
		 BEGIN
		   UPDATE orders SET status='sourcing' WHERE id = NEW.order_id AND status='pending';
		   RETURN NEW;
		 END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER test_steal_child AFTER INSERT ON reservations
		 FOR EACH ROW WHEN (NEW.resource_kind='occupancy')
		 EXECUTE FUNCTION test_steal_child()`,
	}
	for _, s := range stmts {
		if _, err := db.DB.Exec(s); err != nil {
			t.Fatalf("install steal-child trigger: %v", err)
		}
	}
}

// TestAdvanceCompound_CASLoserDoesNotClearWinnersOccupancy
//
// THE ROW IS SHARED, WHICH IS WHY THE LOSER MUST NOT TOUCH IT. AcquireOccupancy
// de-duplicates on (order_id, node_id) (store/reservations/mouth.go:319-334), and
// two callers contending for ONE child carry the SAME order_id. There is exactly
// one occupancy row, inserted by whichever caller got there first, and it is the
// winner's. ReleaseLaneOccupancy is order-keyed and deletes every occupancy row
// the order holds (lane_gate.go:289-293 → mouth.go:353-360), so a loser that
// releases deletes the only row there is.
//
// The winner then dispatches into a lane that reads EMPTY to the next leg's
// admission check — which is the collision Hold B exists to prevent, and it
// matters more on this branch than it would have before: 488729e0 retired the
// sibling-in-flight guard, so the occupancy row is now the thing keeping two legs
// out of one lane.
//
// MUTATION: drop the IsConcurrentTransition guard in AdvanceCompoundOrder (make
// the release unconditional again) and this fails on the occupants assertion —
// the lane reads empty with the winner inside it.
func TestAdvanceCompound_CASLoserDoesNotClearWinnersOccupancy(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	parent, children, lane, _ := twoLegCompound(t, db, "CASREL")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	stealChildAtOccupancyInsert(t, db)

	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance: %v", err)
	}

	// The child whose CAS was lost is the one the trigger stole: leg one.
	occ, err := reservations.OccupantsOf(db.DB, lane)
	if err != nil {
		t.Fatalf("occupants of lane %d: %v", lane, err)
	}
	if len(occ) != 1 || occ[0] != children[0].ID {
		t.Fatalf("lane occupants = %v, want exactly leg one (%d). The losing caller deleted the "+
			"WINNER's occupancy row — they share an order_id, so there is only one — and the lane "+
			"now reads empty to the next leg's admission check while the winner is inside it",
			occ, children[0].ID)
	}
}

// TestAdvanceCompound_StatusWriteFailureStillReleasesOccupancy is the other half,
// and it is the reason the guard is scoped to ConcurrentTransition rather than
// made an unconditional "never release".
//
// A transient failure on the status write is the arm that WEDGES: nothing else
// clears the row. Terminalization releases by order and the CAS-loss arm has a
// winner that consumes the row, but a child left `pending` holding its own
// occupancy is a lane that reads busy — busy with the very leg trying to enter —
// and no re-drive can clear it. AdvanceCompoundOrder's own comment (compound.go)
// states the rule: a hold is only fail-closed if the thing held can be released.
//
// The trigger here raises on the status write itself, so MoveToSourcing returns a
// plain DB error rather than ConcurrentTransition.
//
// MUTATION: widen the guard to `if false` (never release) and this fails — the
// lane stays occupied by a child that was never dispatched.
func TestAdvanceCompound_StatusWriteFailureStillReleasesOccupancy(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	parent, _, lane, _ := twoLegCompound(t, db, "WRFAIL")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	stmts := []string{
		`CREATE OR REPLACE FUNCTION test_block_sourcing() RETURNS trigger AS $$
		 BEGIN
		   RAISE EXCEPTION 'injected status-write failure';
		 END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER test_block_sourcing BEFORE UPDATE ON orders
		 FOR EACH ROW WHEN (NEW.status='sourcing')
		 EXECUTE FUNCTION test_block_sourcing()`,
	}
	for _, s := range stmts {
		if _, err := db.DB.Exec(s); err != nil {
			t.Fatalf("install block-sourcing trigger: %v", err)
		}
	}

	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance: %v", err)
	}

	occ, err := reservations.OccupantsOf(db.DB, lane)
	if err != nil {
		t.Fatalf("occupants of lane %d: %v", lane, err)
	}
	if len(occ) != 0 {
		t.Fatalf("lane occupants = %v, want none. The status write failed, so nothing was dispatched "+
			"and nothing is inside the lane — but the occupancy row survived, and no re-drive can "+
			"clear it: the lane is now permanently busy with a leg that never left `pending`", occ)
	}
}
