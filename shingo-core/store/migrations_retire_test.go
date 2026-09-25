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
// v90 is the maintained-group config surface (two node-keyed set tables,
// bin_types.length_in, mtimes on node_properties/node_bin_types). Appended below
// v89, which is where the head has to stay.
//
// v91 drops bin_loaders.buffer_dest, the loader staging group retired in the
// same release. The column was blank at every plant — a live census confirmed it
// before the retirement landed — so this removes a column, not data. It is the
// one migration here that an OLDER binary cannot survive: pre-retirement Core
// names buffer_dest in loaderCols, so every loader read fails against a database
// this has run on. Rolling back needs the column re-added first, which is exact
// and is one statement because every value was blank.
//
// v92 drops production_log, the write-only shadow of bin_uop_ledger's delta rows
// since the §14 cutover. The baseline's CREATE went with it — see the v92 entry
// — and rollback is self-healing (a pre-v92 binary re-creates the table empty
// from its own baseline), so unlike v91 nothing manual stands between a plant
// and its .previous binary.
//
// v93 CREATES bin_uop_exception, the permanent exceptions ledger (owner
// decision D2: negatives and boundaries are durable; the raw stream they were
// derived from is not — P4 starts deleting it at 90 days in this same wave).
// The first migration since v89 that ADDS to the schema rather than retiring
// from it, and the first whose backfill is the point rather than a repair:
// the crossings/drops/boundaries it derives from bin_uop_ledger are exactly
// what the 90-day purge would otherwise make un-derivable. Rollback is inert
// (a pre-v93 binary never reads or writes the table) but lossy to redo —
// dropping the table destroys the backfill, so .previous against a purged
// database cannot get this data back. That is the D2 trade, accepted.
//
// v94 CREATES bin_uop_delta_daily, the permanent daily roll-up of the raw
// delta stream (owner decision D3: roll-up growth accepted, ~10 rows/day).
// Same family and same rollback doctrine as v93: inert to a pre-v94 binary,
// and the backfill — the daily part-flow history — is the thing that must
// not be lost. Together v93+v94 are the durable half of the D6 retention
// trade: after they exist, deleting raw deltas at 90 days (P4) destroys
// nothing the owner named durable.
//
// v97 ADDS orders.key_route / key_task — the SEER routing hints an Edge claim
// configures, carried on the order because intake and dispatch are separated by
// time. Additive columns with ” defaults: inert to a pre-v97 binary, which
// simply never reads them, and there is no backfill to lose.
//
// v115 ADDS order_intake_refusals — a new table only the pair rule reads. Inert
// to a pre-v115 binary, which never looks at it, and empty at birth.
//
// v116 ADDS three payloads columns and bin_types.required_robot_group — the
// near-empty robot group relaxation. All additive with false/”/0 defaults, so
// a pre-v116 binary reads the same group it always did and there is no backfill
// to lose.
//
// v117 ADDS bins.undeclared_carrier_at — the carrier-rule finding the produce
// door records instead of refusing a load that already happened. Nullable, no
// backfill, and inert to a pre-v117 binary: nothing older reads the column, and
// a plant that rolls back simply stops recording findings rather than losing
// any — the stamp is re-derived at the next payload write on each bin. It was
// written as 116 on a branch cut from a 115 head and renumbered when that
// branch met main, which had already shipped 116 to Hopkinsville.
//
// v118 ADDS tte_samples — the per-line time-to-empty the sourceability pass
// already computes on every full pass and has always discarded. A new table
// nothing older reads, empty at birth, written only by the two-minute full
// recompute and self-pruning at 45 days. Inert to a pre-v118 binary, and a
// plant that rolls back stops accumulating samples rather than losing any
// verdict: no surface reads it, so nothing on the wire depends on it existing.
//
// v119 ADDS bin_uop_ledger.reason + tte_samples.rate_grain — two additive
// columns with defaults, plus a one-time backfill of reason from the metadata
// JSON key the applier already writes, bounded to 7 days. Inert to a pre-v119
// binary; a rollback drops both columns and the backfill is never needed
// again (metadata keeps the reason — one fact, two homes for one release).
//
// v120 ADDS lineside_drain_ledger - the lineside drain own row shape. The
// drain is consumption at a node from a pile, not a bin event: bin_uop_ledger
// cannot take it (bin_id NOT NULL, nine non-nullable scans), and the bucket
// row deletes at qty 0 so no history survives there. One row per applied
// consume_drain, written in ApplyLinesideBucketDelta own transaction; the
// consumption rate reads it through a UNION ALL arm. Inert to a pre-v120
// binary; rollback is DROP TABLE.
//
// v121 ADDS four style_claims columns — the claim's legs, the nodes it draws
// from, ships to and is paired with. All four are TEXT, NOT NULL with an
// empty-string default, so a pre-v121 binary's INSERT, which names none of
// them, still lands; it simply
// stops recording legs. No backfill is possible and none is wanted: the mirror
// is replaced wholesale per process on every plant-claims message, so every
// process repopulates at its next publish.
//
// v122 ADDS tte_samples.kind — which kind of demand episode a kept projection
// is to be scored against. TEXT NOT NULL DEFAULT 'cell', and the default IS the
// backfill: every row written before it is a cell sample by construction, the
// only writer having been the running style's claims. Inert to a pre-v122
// binary, which never names the column and whose rows the default describes
// correctly; rollback is DROP COLUMN and costs no row, only the ability to tell
// a loader's projection from a cell's.
//
// v123 ADDS bin_loaders.accept_partials — an unloader may be fed partly
// drained carriers. BOOLEAN NOT NULL DEFAULT false, and the default keeps every
// existing unloader on the full-carrier rule. Inert to a pre-v123 binary;
// rollback is DROP COLUMN and returns every unloader to fulls-only.
//
// v124 ADDS bin_types.bare and bin_loaders.bare_bin_type_id — the half
// loader's carrier type and the unloader that stamps it. DEFAULT false and
// NULL keep every type sourceable and every unloader stamping nothing. Inert to
// a pre-v124 binary; rollback is DROP COLUMN on both.
//
// v125 ADDS bin_loaders.auto_push — an unloader re-pulls its next full when a
// window frees. DEFAULT false is every Core-owned unloader's live behaviour.
// Inert to a pre-v125 binary; rollback is DROP COLUMN.
//
// v126 ADDS inventory_delta_dedup.applied_net and applied_window_end — the
// running net's anchor and the rollback detector's last applied window. NULL
// on every existing row, which is the mixed-version anchor. Inert to a
// pre-v126 binary; rollback is DROP COLUMN on both.
//
// v127 ADDS edge_lineside_reports.bin_id/bin_epoch/flushed_seq (the report's
// carrier, all NULL on an old Edge's row) and DROPS NOT NULL on
// bin_uop_exception.bin_id, so a bucket report_divergence can be recorded.
// Catalog-only; inert to a pre-v127 binary.
//
// v131 reshapes lineside_buckets for the level wire (truncated; style_id and
// pair_key out; state in; keyed (core_node_name, payload_code, state)), deletes
// the retired "bucket" dedup rows, drops four unread drain-ledger columns and
// closes open bucket report_divergence episodes with a reason. v132 drops
// demand_origins.used_edge_reports. Neither is inert to an older binary: brief
// v7 rules out a rollback to the previous build.
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
	if got := store.LatestMigrationVersion(); got != 132 {
		t.Errorf("head migration = %d, want 132", got)
	}
}

// TestMigrate_CorrectionsRetired: v104 drops the corrections table, which
// nothing in the tree has ever written or read.
//
// The second half is the one worth having. A dropped table that the BASELINE
// still creates comes back on the next fresh install, so the migration and the
// baseline have to agree — that is the shape v92 called out for production_log,
// and it is checked here by migrating twice rather than by reading the DDL: the
// second pass runs the baseline again over a migrated database.
func TestMigrate_CorrectionsRetired(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	if schema.TableExists(db.DB, "corrections") {
		t.Error("corrections must be dropped by v104")
	}
	if err := db.MigrateForTest(); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if schema.TableExists(db.DB, "corrections") {
		t.Fatal("corrections came back on a second pass — the baseline DDL still creates it, " +
			"so a fresh install would ship the table the migration exists to remove")
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
