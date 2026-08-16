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
// restore-blockers table is gone (v70) and the head version is current (77, after
// the refactor-phase1 merge brought v69/v70 in under main's v71–v75).
//
// The drop was v52 on refactor-phase1 and became v70 at transplant — main had
// claimed 52 and run on to 68. The head-version assertion is the one that would
// have caught a missed renumber loudly: latestMigrationVersion is read off the
// LAST list element, so a drop left at 52 would report the head as 52 while
// later migrations sat above it. After the catch-up merge, main's loader/index
// migrations (v71–v75) sit above v70, and v76 (the lane-occupancy resource_kind)
// above those, so the head is 76 rather than 70. v78 dropped
// pending_lane_extensions, the expose bridge's table, with the machinery that
// read it; v79 added orders.dig_target_node, the slot a service dig uncovers;
// v80 deduped order_bins and enforced one row per (order, bin), the grain the
// junction always claimed and never had; v81 — demand_origins episodes become
// produce/consume instead of supply/evacuate, retiring the second vocabulary for
// the claim's own role — is the current head. v81 is a DATA migration: it alters
// no schema object, which is why its post-condition reads rows rather than
// asking ColumnExists.
//
// THIS NUMBER IS MEANT TO BE EDITED, once, by whoever adds a migration. It is
// not a value to sync -- it is the second person confirming the head moved on
// purpose, which is the only thing that distinguishes "v79 was added" from
// "v79 was added below v78 and the head silently did not move".
func TestMigrate_PendingRestocksRetired(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	if schema.TableExists(db.DB, "pending_restocks") {
		t.Error("pending_restocks must be dropped by v70")
	}
	if got := store.LatestMigrationVersion(); got != 81 {
		t.Errorf("head migration = %d, want 81", got)
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
