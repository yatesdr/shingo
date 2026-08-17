//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/schema"
)

// TestMigrate_PendingRestocksRetired: after the full migration chain the retired
// restore-blockers table is gone (v70) and the head version is the last element
// of the list.
//
// ── THE INSTRUMENT, AND THE SECOND TIME IT HAS BEEN USED ──────────────────
//
// The drop was v52 on refactor-phase1 and became v70 at transplant — main had
// claimed 52 and run on to 68. The head-version assertion is the one that would
// have caught a missed renumber loudly: latestMigrationVersion is read off the
// LAST list element, not the maximum, so a migration left at a low number
// reports that low number as the head while later ones sit above it.
//
// THIS RENUMBER IS THE SECOND TRANSPLANT, and it is bigger than the first. Both
// plants run origin/main at schema 83; this branch has never been to a plant.
// The two trees had assigned 77–82 to six pairs of unrelated migrations, so the
// branch's work moved to 84–88 (77→84 open_for_children, 78→85 the
// pending_lane_extensions drop, 80→86 the order_bins dedupe, 81→87 the
// episode-role rewrite, 82→88 destination_resolved_at). The branch's old v79 is
// not in that list: it added orders.dig_target_node, which the same batch
// deleted, so the migration was removed rather than renumbered.
//
// v76 KEPT ITS NUMBER, deliberately. main has no v76 — it skipped the number and
// left a note saying the lane-occupancy migration on this branch holds it — so
// there is no collision to resolve and no row for it at either plant. The
// migrator tests membership per version (`WHERE version = $1`), not `> max`, so
// an unrecorded 76 runs once, in list order, exactly like the 84–88 block.
//
// v73 IS MAIN'S NOW. Both trees had recorded a v73 and they were OPPOSITE: main
// adds the `restore-%` exemption to idx_orders_uuid, this branch removed it.
// Since a plant ran main's, 73 means main's, and the removal became v89 at the
// end. v73's post-condition is retired to always-true for the same reason v23's
// and v24's are — a live post-condition that a later migration undoes makes the
// two re-run each other on every boot.
//
// THIS NUMBER IS MEANT TO BE EDITED, once, by whoever adds a migration. It is
// not a value to sync -- it is the second person confirming the head moved on
// purpose, which is the only thing that distinguishes "a migration was added"
// from "a migration was added below the head and the head silently did not
// move".
func TestMigrate_PendingRestocksRetired(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	if schema.TableExists(db.DB, "pending_restocks") {
		t.Error("pending_restocks must be dropped by v70")
	}
	if got := store.LatestMigrationVersion(); got != 89 {
		t.Errorf("head migration = %d, want 89", got)
	}
}

// TestMigrate_RetiredTableNotResurrected: a resurrected/stray pending_restocks is
// removed by a migration pass (v70 self-heal) and v23's always-true verify does
// NOT re-create it. The framework must never resurrect a retired table.
func TestMigrate_RetiredTableNotResurrected(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	if _, err := db.Exec(`CREATE TABLE pending_restocks (id BIGSERIAL PRIMARY KEY)`); err != nil {
		t.Fatalf("recreate stray table: %v", err)
	}
	if err := db.MigrateForTest(); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if schema.TableExists(db.DB, "pending_restocks") {
		t.Fatal("a migration pass must drop a resurrected pending_restocks and never re-create it")
	}
}

// TestRetireReshuffleRestore_CancelsStray: the boot sweep cancels a non-terminal
// reshuffle_restore parent and its non-terminal child, and is idempotent.
func TestRetireReshuffleRestore_CancelsStray(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = protocol.OrderTypeReshuffleRestore
		o.Status = protocol.StatusReshuffling
	})
	testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.ParentOrderID = &parent.ID
		o.Status = protocol.StatusQueued
	})

	n, err := db.RetireReshuffleRestoreOrders()
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if n != 2 {
		t.Fatalf("cancelled = %d, want 2 (parent + child)", n)
	}
	got, err := db.GetOrder(parent.ID)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if got.Status != protocol.StatusCancelled {
		t.Errorf("parent status = %q, want cancelled", got.Status)
	}
	// Idempotent: a second run finds nothing.
	if n2, _ := db.RetireReshuffleRestoreOrders(); n2 != 0 {
		t.Errorf("second run cancelled %d, want 0 (idempotent)", n2)
	}
}

// TestRetireReshuffleRestore_NoOpClean: a DB with no reshuffle_restore orders is
// untouched.
func TestRetireReshuffleRestore_NoOpClean(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.CreateOrder(t, db) // a normal retrieve — must be left alone
	n, err := db.RetireReshuffleRestoreOrders()
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if n != 0 {
		t.Fatalf("cancelled = %d on a clean DB, want 0", n)
	}
}

// TestMigrate_NoMigrationOscillatesOnEveryBoot pins the rule the v24/v78 pair
// broke, rather than just that pair.
//
// ── THE FAILURE ───────────────────────────────────────────────────────────
//
// A CREATE migration whose verify asserts its table EXISTS, retired later by a
// DROP whose verify asserts it does NOT, gives the self-heal two recorded-applied
// migrations with mutually exclusive post-conditions. Every boot re-runs both:
// the create re-creates, the drop re-drops. The end state is right — they are
// idempotent and the drop runs last — which is exactly why it survived.
//
// What it costs is the self-heal's ONLY alarm. "recorded as applied but
// post-condition fails" is how a genuinely missing migration announces itself,
// and printing it twice on every healthy boot is how a reader learns to skip it.
// Observed on the rig 2026-08-14, two lines in the first three of a run log.
//
// ── THE ASSERTION ─────────────────────────────────────────────────────────
//
// After a full migration chain, a SECOND pass must re-run nothing. That is the
// general property; it catches this pair and any future one, without this test
// needing to know which tables are retired.
//
// MUTATION (verified): restore v24's TableExists verify and this fails naming
// both v24 and v78.
func TestMigrate_NoMigrationOscillatesOnEveryBoot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t) // the full chain has already run once here

	// A second pass over an already-migrated database must find every recorded
	// migration's post-condition satisfied, and therefore re-run none of them.
	// MigrateForTest logs a "re-running" line per failure; the observable we can
	// assert on is the verify set itself.
	stale := db.MigrationsFailingTheirPostCondition()
	if len(stale) > 0 {
		t.Errorf("%d migration(s) report applied-but-post-condition-failing on a freshly migrated "+
			"database, so every boot re-runs them: %v\n\n"+
			"A CREATE whose verify asserts its table exists, retired by a DROP whose verify asserts "+
			"it does not, oscillates forever. The end state stays correct, so the only cost is the "+
			"self-heal's alarm — and an alarm that fires on every healthy boot is one nobody reads. "+
			"The CREATE stops asserting a state it no longer owns: give it an always-true verify and "+
			"say so in its title, as v23 does for v70.", len(stale), stale)
	}
}
