// migrations.go — Edge SQLite migration runner.
//
// Phase 6.0b extracted this from the 1013-line schema.go. Layout:
//
//   schema/sqlite_ddl.go     — canonical "fresh DB" CREATE TABLE constant
//   schema/schema.go         — Apply() + introspection helpers
//   migrations.go (this file) — legacyDropDDL constant, migrate() entry
//                              point, per-table rename/rebuild/strip
//                              helpers, db.tableHasColumn wrapper (kept
//                              for migration_test.go's existing call
//                              sites).
//
// All migrations are idempotent — safe to re-run on an already-migrated
// DB. New columns are added via ALTER TABLE ... ADD COLUMN (SQLite
// silently fails on duplicates, which we ignore). Structural changes
// use the rename-rebuild pattern: rename existing → CREATE new →
// INSERT INTO ... SELECT → DROP old. Versioned per-column migrations
// run AFTER schema.Apply().

package store

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	"shingoedge/store/schema"
)

// hasV5LoaderColumn returns true when loader_payload_thresholds exists
// with the v5 loader_node_id column. SQLite's pragma_table_info is the
// portable column probe.
func hasV5LoaderColumn(db *DB) bool {
	var hit int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('loader_payload_thresholds') WHERE name='loader_node_id'`).Scan(&hit)
	if err != nil {
		return false
	}
	return hit > 0
}

// hasLegacyLoaderCacheKey reports whether the Core loader cache is on the old
// (core_node_name, role) shape (pre-6b), detected by the core_node_name column on
// core_loaders. When true, migrate() drops the three cache tables so schema.Apply
// recreates them loader_key-keyed; the cache is rebuilt full-state on the next sync.
func hasLegacyLoaderCacheKey(db *DB) bool {
	var hit int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('core_loaders') WHERE name='core_node_name'`).Scan(&hit)
	if err != nil {
		return false
	}
	return hit > 0
}

// hasLegacyInventoryDeltaSeqPK returns true when inventory_delta_seq
// exists without the epoch column (and therefore without epoch in its
// PK). Probe via pragma_table_info — pre-epoch rows have just
// scope_kind / scope_key / next_seq / updated_at.
func hasLegacyInventoryDeltaSeqPK(db *DB) bool {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('inventory_delta_seq')`).Scan(&n); err != nil || n == 0 {
		return false
	}
	var hasEpoch int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('inventory_delta_seq') WHERE name='epoch'`).Scan(&hasEpoch); err != nil {
		return false
	}
	return hasEpoch == 0
}

// legacyDropDDL drops tables that have been removed entirely from the
// canonical schema. Runs first in migrate() so the rest of the
// migration logic operates on a clean table set. DROP IF EXISTS is
// safe on every database state.
//
// kanban_calculations / threshold_calculations were two short-lived
// v5/v6 audit-trail experiments — the v6 fix-up pass dropped them
// after deciding the engineer doesn't need a per-calculate history
// (current source / updated_at / updated_by on the threshold row
// covers "what's the current value based on", and re-running
// Calculate is the way to see fresh inputs). Listed here so any dev
// DB that has either name gets it cleaned up.
const legacyDropDDL = `
DROP TABLE IF EXISTS bom_entries;
DROP TABLE IF EXISTS inventory;
DROP TABLE IF EXISTS materials;
DROP TABLE IF EXISTS kanban_templates;
DROP TABLE IF EXISTS operator_screens;
DROP TABLE IF EXISTS kanban_calculations;
DROP TABLE IF EXISTS threshold_calculations;
`

// migrate runs the full forward-migration pipeline: legacy DROPs,
// legacy table renames, canonical CREATE (via schema.Apply), per-
// column renames/rebuilds, idempotent ALTER ADD COLUMN, and data
// fixups. Callable on databases of any age — every step is no-op on
// already-migrated state.
func (db *DB) migrate() error {
	// 1. Cleanup: drop tables removed from the canonical schema
	if _, err := db.Exec(legacyDropDDL); err != nil {
		return err
	}
	db.Exec("ALTER TABLE orders DROP COLUMN material_id")

	// UOP-threshold replenishment v5→v6 schema replace: the branch
	// isn't merged, so an in-place drop+recreate of
	// loader_payload_thresholds is acceptable when the v5 column
	// shape is detected. SQLite has no generic "table has column X
	// with type Y" check, so we probe for the v5 loader_node_id
	// column specifically — if it's there, this DB is on v5 schema
	// and we drop the table so schema.Apply rebuilds the v6 shape.
	// The audit-table siblings (kanban_calculations,
	// threshold_calculations) are dropped unconditionally via
	// legacyDropDDL above; both were short-lived experiments and the
	// fix-up pass removed them entirely.
	if hasV5LoaderColumn(db) {
		db.Exec("DROP TABLE IF EXISTS loader_payload_thresholds")
	}

	// The Core loader cache (core_loaders + children) re-keyed from
	// (core_node_name, role) to loader_key when the loader's borrowed node id was
	// dropped. SQLite can't ALTER PRIMARY KEY in place, and the cache is rebuilt
	// full-state on the next node-list sync, so detect the old shape (a
	// core_node_name column on core_loaders) and DROP the three tables —
	// schema.Apply recreates them loader_key-keyed and the next sync repopulates.
	if hasLegacyLoaderCacheKey(db) {
		db.Exec("DROP TABLE IF EXISTS core_loader_positions")
		db.Exec("DROP TABLE IF EXISTS core_loader_payloads")
		db.Exec("DROP TABLE IF EXISTS core_loaders")
	}

	// 2. Rename legacy tables BEFORE schema.Apply so existing data
	//    migrates into the new table names instead of being orphaned
	//    behind a freshly-created empty replacement.
	db.Exec("ALTER TABLE production_lines RENAME TO processes")
	db.Exec("ALTER TABLE job_styles RENAME TO styles")
	db.Exec("ALTER TABLE location_nodes RENAME TO nodes")
	// transitional_loaders → operator_driven_loaders: "transitional" read like a
	// changeover/temporary state when it just means operator-driven replenishment.
	// Same set, clearer name. Renamed before schema.Apply so existing rows migrate
	// into the new table rather than being orphaned behind a fresh empty one.
	// No-op on a fresh DB (old table absent → ALTER errors, ignored).
	db.Exec("ALTER TABLE transitional_loaders RENAME TO operator_driven_loaders")

	// 3. Canonical CREATE TABLE IF NOT EXISTS pass.
	if err := schema.Apply(db.DB); err != nil {
		return err
	}

	// 4. Per-table column renames (graceful rebuilds).
	if err := db.migrateProcessColumns(); err != nil {
		return err
	}
	if err := db.migrateStyleColumns(); err != nil {
		return err
	}
	if err := db.stripDeadStyleColumns(); err != nil {
		return err
	}
	if err := db.migrateReportingPointColumns(); err != nil {
		return err
	}
	if err := db.migrateHourlyCountColumns(); err != nil {
		return err
	}
	if err := db.migrateHourlyCountFKs(); err != nil {
		return err
	}
	if err := db.migrateOperatorStationColumns(); err != nil {
		return err
	}
	if err := db.migrateProcessNodeOwnership(); err != nil {
		return err
	}
	if err := db.stripLegacyRuntimeStateColumns(); err != nil {
		return err
	}
	if err := db.renameRemainingUOPCached(); err != nil {
		return err
	}
	// Runs AFTER the process_nodes column migrations (it reads core_node_name and
	// operator_station_id) and creates the UNIQUE(process_id, core_node_name) index
	// once the rows can satisfy it.
	if err := db.collapseDuplicateProcessNodes(); err != nil {
		return err
	}

	// Remove vestigial loaded_bin_label and loaded_at columns (safe no-ops if absent)
	db.Exec("ALTER TABLE process_node_runtime_states DROP COLUMN loaded_bin_label")
	db.Exec("ALTER TABLE process_node_runtime_states DROP COLUMN loaded_at")

	// Bin-ownership flip: active_bin_id is the canonical "what bin is at
	// this slot" pointer (Edge-owned while the bin is at lineside).
	// Idempotent — duplicate ALTER ADD COLUMN fails silently in SQLite.
	db.Exec("ALTER TABLE process_node_runtime_states ADD COLUMN active_bin_id INTEGER")

	// Retire the cached_bin_id gap-window second pointer. The old
	// two-pointer model gated cache writes on active_bin_id == cached_bin_id
	// (steady state) and suppressed them during the release→delivery gap;
	// the single-pointer hold-and-replay model (pending_uop_delta below)
	// replaces it. DROP is a no-op on fresh DBs that never had the column
	// (this whole migration re-runs every startup and ignores errors) and
	// removes the column from existing DBs that were upgraded through the
	// old ADD COLUMN.
	db.Exec("ALTER TABLE process_node_runtime_states DROP COLUMN cached_bin_id")

	// Epoch mirror for active_bin_id (post-epoch fix). Old rows land
	// at 0 — the pre-migration cohort that lines up with Core's
	// backfilled inventory_delta_dedup rows. The next bin lifecycle
	// event on Core writes a non-zero value here via the LoadBin /
	// FetchNodeBins response path.
	db.Exec("ALTER TABLE process_node_runtime_states ADD COLUMN active_bin_epoch INTEGER NOT NULL DEFAULT 0")

	// Hold-and-replay: pending tick counts accumulated while no bin is
	// bound at the slot (pickup->delivery gap). Replaces the cached_bin_id
	// gap-window. Old rows land at 0 (no pending). Idempotent ADD COLUMN.
	db.Exec("ALTER TABLE process_node_runtime_states ADD COLUMN pending_uop_delta INTEGER NOT NULL DEFAULT 0")

	// Drop zombie nodes table (replaced by process_nodes + core node sync)
	db.Exec("DROP TABLE IF EXISTS nodes")

	// Drop tables replaced by style_node_claims
	db.Exec("DROP TABLE IF EXISTS process_node_style_assignments")
	db.Exec("DROP TABLE IF EXISTS op_node_style_assignments_legacy")
	db.Exec("DROP TABLE IF EXISTS changeover_log")

	// 5. ALTER TABLE additions (idempotent — duplicate column adds fail
	//    silently in SQLite and we deliberately ignore the error).
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN swap_mode TEXT NOT NULL DEFAULT 'simple'")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN staging_node TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN release_node TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN keep_staged INTEGER NOT NULL DEFAULT 0")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN evacuate_on_changeover INTEGER NOT NULL DEFAULT 0")

	// Rename staging_node → inbound_staging, release_node → outbound_staging on style_node_claims
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN inbound_staging TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN outbound_staging TEXT NOT NULL DEFAULT ''")
	db.Exec("UPDATE style_node_claims SET inbound_staging = staging_node WHERE staging_node != ''")
	db.Exec("UPDATE style_node_claims SET outbound_staging = release_node WHERE release_node != ''")

	// Source / destination routing on style_node_claims (collapsed from node/node_group pairs)
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN inbound_source_node TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN inbound_source_node_group TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN outbound_source_node TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN outbound_source_node_group TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN inbound_source TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN outbound_source TEXT NOT NULL DEFAULT ''")
	db.Exec("UPDATE style_node_claims SET inbound_source = COALESCE(NULLIF(inbound_source_node, ''), inbound_source_node_group) WHERE inbound_source = ''")
	db.Exec("UPDATE style_node_claims SET outbound_source = COALESCE(NULLIF(outbound_source_node, ''), outbound_source_node_group) WHERE outbound_source = ''")

	// Allowed payload codes and auto-request for manual_swap nodes
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN allowed_payload_codes TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN auto_request_payload TEXT NOT NULL DEFAULT ''")

	// Edge-local per-part cycle time used by the threshold calculator.
	// Not synced from Core (see catalog.UpsertCatalog comment).
	db.Exec("ALTER TABLE payload_catalog ADD COLUMN cycle_seconds REAL NOT NULL DEFAULT 0")

	// Multi-window C0: explicit position kind on the cached loader aggregate
	// ('window' | 'dedicated'), synced from Core's LoaderPosition.Kind, so the
	// resolver never sniffs an empty payload to tell a shared-window window from
	// an unassigned dedicated position. Idempotent — duplicate ADD COLUMN fails
	// silently. Empty on rows from a pre-Kind Core; the parent loader's layout
	// stays authoritative (C1 branches on Layout).
	db.Exec("ALTER TABLE core_loader_positions ADD COLUMN kind TEXT NOT NULL DEFAULT ''")

	// Loader identity cutover (step 4): the cached loader's opaque identity token
	// ("loader:<id>"), synced from Core's LoaderInfo.LoaderKey. projectCoreLoader keys
	// the loader's identity on this instead of core_node_name. Idempotent — duplicate
	// ADD COLUMN fails silently; empty on rows from a pre-cutover Core, in which case
	// projectCoreLoader falls back to core_node_name.
	db.Exec("ALTER TABLE core_loaders ADD COLUMN loader_key TEXT NOT NULL DEFAULT ''")

	// 6. Data fixups
	db.Exec("UPDATE orders SET status='pending' WHERE status='queued'")

	// Legacy catalog renames
	db.Exec("ALTER TABLE style_catalog RENAME TO blueprint_catalog")
	db.Exec("ALTER TABLE blueprint_catalog DROP COLUMN form_factor")
	db.Exec("ALTER TABLE blueprint_catalog RENAME TO payload_catalog")

	// Legacy order columns
	db.Exec("ALTER TABLE orders ADD COLUMN steps_json TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE orders ADD COLUMN staged_expire_at TEXT")
	// Core's bin id snapshot at delivery — used for PLC tick
	// attribution (BinUOPDelta).
	db.Exec("ALTER TABLE orders ADD COLUMN bin_id INTEGER")
	db.Exec("ALTER TABLE orders ADD COLUMN process_node_id INTEGER REFERENCES process_nodes(id) ON DELETE SET NULL")
	// Index must come after ALTER in case legacy orders table lacked the column
	db.Exec("CREATE INDEX IF NOT EXISTS idx_orders_process_node_id ON orders(process_node_id)")
	// source_node was unindexed, so BuildView's per-node
	// ListActiveOrdersByProcessNodeOrSource OR-query (process_node_id OR
	// source_node) table-scanned the orders table on every SSE refresh.
	// Indexing source_node lets SQLite use an OR-by-union plan instead.
	db.Exec("CREATE INDEX IF NOT EXISTS idx_orders_source_node ON orders(source_node)")

	// WarLink tag management tracking
	db.Exec("ALTER TABLE reporting_points ADD COLUMN warlink_managed INTEGER NOT NULL DEFAULT 0")

	// Counter config columns on processes
	db.Exec("ALTER TABLE processes ADD COLUMN counter_plc_name TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE processes ADD COLUMN counter_tag_name TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE processes ADD COLUMN counter_enabled INTEGER NOT NULL DEFAULT 0")

	// Migrate counter binding data to process columns
	if exists, _ := schema.TableExists(db.DB, "process_counter_bindings"); exists {
		db.Exec(`UPDATE processes SET
			counter_plc_name = COALESCE((SELECT plc_name FROM process_counter_bindings WHERE process_id = processes.id), ''),
			counter_tag_name = COALESCE((SELECT tag_name FROM process_counter_bindings WHERE process_id = processes.id), ''),
			counter_enabled = COALESCE((SELECT enabled FROM process_counter_bindings WHERE process_id = processes.id), 0)`)
		db.Exec("DROP TABLE process_counter_bindings")
	}

	// Auto-create default process if styles exist but no processes do
	var processCount int
	db.QueryRow("SELECT COUNT(*) FROM processes").Scan(&processCount)
	if processCount == 0 {
		var styleCount int
		db.QueryRow("SELECT COUNT(*) FROM styles").Scan(&styleCount)
		if styleCount > 0 {
			db.Exec("INSERT INTO processes (name, description) VALUES ('Line 1', 'Default production process')")
			db.Exec("UPDATE styles SET process_id = (SELECT id FROM processes WHERE name = 'Line 1') WHERE process_id IS NULL")
		}
	}

	// Rename pickup_node → source_node on orders (aligns with protocol SourceNode)
	db.Exec("ALTER TABLE orders RENAME COLUMN pickup_node TO source_node")

	// Rename outbound_source → outbound_destination on style_node_claims (it's a dropoff destination, not a source)
	db.Exec("ALTER TABLE style_node_claims RENAME COLUMN outbound_source TO outbound_destination")

	// A/B node cycling: paired_core_node on claims, active_pull on runtime
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN paired_core_node TEXT NOT NULL DEFAULT ''")

	// Claim-level auto-confirm: allows manual_swap claims to auto-confirm delivery
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN auto_confirm INTEGER NOT NULL DEFAULT 0")
	db.Exec("ALTER TABLE process_node_runtime_states ADD COLUMN active_pull INTEGER NOT NULL DEFAULT 1")

	// Bin loader/unloader mode: "loader" (default) or "unloader"
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN mode TEXT NOT NULL DEFAULT 'loader'")

	// v15: Convert bin_loader role to produce/consume with manual_swap.
	// The mode column stays as a harmless vestige — code no longer reads it.
	db.Exec(`UPDATE style_node_claims SET role = 'produce', swap_mode = 'manual_swap' WHERE role = 'bin_loader' AND (mode = 'loader' OR mode = '')`)
	db.Exec(`UPDATE style_node_claims SET role = 'consume', swap_mode = 'manual_swap' WHERE role = 'bin_loader' AND mode = 'unloader'`)

	// v15 backfill: manual_swap claims need allowed_payload_codes populated.
	// Seed from the legacy single payload_code for any manual_swap claim that was
	// converted before this backfill existed (otherwise the edit modal's picker
	// is empty and Save rejects with "Select at least one allowed payload").
	db.Exec(`UPDATE style_node_claims
		SET allowed_payload_codes = '["' || payload_code || '"]'
		WHERE swap_mode = 'manual_swap'
		  AND (allowed_payload_codes = '' OR allowed_payload_codes = '[]')
		  AND payload_code <> ''`)

	// v16: Add payload_code to orders for per-payload demand mapping.
	db.Exec("ALTER TABLE orders ADD COLUMN payload_code TEXT NOT NULL DEFAULT ''")

	// v17 (lineside phase 6): per-claim soft threshold for the release
	// qty-override prompt. Zero means "off" — the default. When >0, the
	// HMI warns if the operator enters a qty greater than 2× this value.
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN lineside_soft_threshold INTEGER NOT NULL DEFAULT 0")

	// v18: optional third position for two_robot_press_index. When set,
	// the press indexes C → B → A every cycle and R1's final dropoff
	// goes to C instead of B. Empty string means the legacy 2-position
	// layout (R1 dropoff → B, R2 indexes B → A).
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN second_paired_core_node TEXT NOT NULL DEFAULT ''")

	// v19 reverted: drop_via_staging column removed (idempotent — no-op
	// on fresh DBs that never had the column).
	db.Exec("ALTER TABLE style_node_claims DROP COLUMN drop_via_staging")

	// v20: per-claim opt-in for "reuse compatible bins". When the next
	// style produces the same payload at a press-index node and the
	// physical bin at the node is empty, the planner can skip the swap
	// entirely (treat as Unchanged). Default false preserves the
	// always-swap behaviour.
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN reuse_compatible_bins INTEGER NOT NULL DEFAULT 0")

	// v21: Durable sibling pointer for two-robot swap pairs. The
	// supply-leg ↔ evac-leg linkage was previously encoded only in the
	// volatile process_node_runtime_states slots (active_order_id /
	// staged_order_id), which decay before release fires (e.g.
	// handler_bin_picked_up nulls active_order_id when the supply bin
	// leaves the supermarket). The sibling pointer survives bin pickup,
	// status transitions, and process restarts; ReleaseStagedOrders'
	// gate and the supply-bin manifest guard read it instead of the
	// volatile slots so neither depends on race-prone state.
	db.Exec("ALTER TABLE orders ADD COLUMN sibling_order_id INTEGER REFERENCES orders(id) ON DELETE SET NULL")

	// v22 (AMR trial 2026-05-08): per-process opt-in for PLC-driven
	// auto-cutover. The cutover monitor subscribes to the
	// Changeover_Active tag derived from the process's existing
	// counter_tag_name parent struct. Default off; operators enable
	// via the process editor admin UI checkbox.
	db.Exec("ALTER TABLE processes ADD COLUMN auto_cutover_enabled INTEGER NOT NULL DEFAULT 0")

	// v23 (AMR trial 2026-05-08): audit column for which trigger source
	// drove the changeover row to its current terminal state. One of
	// "operator-hmi" | "plc-auto" | "auto-task-terminal". Empty while
	// in_progress. Differentiates operator-driven cutover from PLC-
	// driven cutover from B.3 auto-completion when investigating
	// post-mortem timing questions.
	db.Exec("ALTER TABLE process_changeovers ADD COLUMN triggered_by TEXT NOT NULL DEFAULT ''")

	// v24 (Hopkinsville 2026-05-14): per-claim opt-in for unloader auto-push.
	// When true on a consume manual_swap claim, Edge fires a U1 retrieve_full
	// whenever the unloader window is free and a full bin of an allowed
	// payload exists in claim.InboundSource — no kanban demand signal
	// required. Default false preserves the existing kanban-driven model.
	// See engine/operator_demand.go MaybePushUnloader for the trigger logic.
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN auto_push INTEGER NOT NULL DEFAULT 0")

	// v25 (2026-05-16, UOP-threshold replenishment Phase 1): source
	// tracking for reorder_point so the engineer UI can show whether a
	// value was hand-typed or applied from the unified calculator.
	// Default 'legacy' = never touched — preserves the silent-inert
	// behaviour for plants that have not opted in.
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN reorder_point_source TEXT NOT NULL DEFAULT 'legacy'")

	// v26 (UOP-threshold replenishment fix-up): track which calculator
	// inputs the engineer overrode on the Calculate that produced the
	// current threshold. Comma-separated snake_case field names; empty
	// when source != 'calculated' or when no inputs were overridden.
	// Idempotent — SQLite ALTER silently no-ops if the column exists.
	db.Exec("ALTER TABLE loader_payload_thresholds ADD COLUMN overridden_inputs TEXT NOT NULL DEFAULT ''")

	// v25b (UOP-threshold replenishment Phase 1): one-shot cleanup for
	// stale RuntimeState pointers — active_order_id / staged_order_id
	// rows that reference terminal orders. The brief flagged this as a
	// data hygiene step that unblocks autoreorder evaluation paths whose
	// CanAcceptOrders guard returns false silently on stale pointers.
	//
	// Strategy: NULL out a pointer iff the referenced order's status is
	// in the terminal set (confirmed, cancelled, failed, skipped). Live
	// orders are left alone. Idempotent — runs every migrate(), no-op
	// when there's nothing to clean. Default to apply mode here; the
	// dry-run variant lives in the admin endpoint (handlers_api_replenishment).
	db.Exec(`UPDATE process_node_runtime_states
		SET active_order_id = NULL
		WHERE active_order_id IN (
			SELECT id FROM orders WHERE status IN ('confirmed','cancelled','failed','skipped')
		)`)
	db.Exec(`UPDATE process_node_runtime_states
		SET staged_order_id = NULL
		WHERE staged_order_id IN (
			SELECT id FROM orders WHERE status IN ('confirmed','cancelled','failed','skipped')
		)`)

	// Round-3 A* (2026-05-21): narrow the lineside-bucket partial
	// unique index from (node_id, style_id, part_number) to
	// (node_id, part_number) WHERE state='active'. The style_id was
	// load-bearing on the original "one bucket per (node, style, part)"
	// model where multi-style transient overlap during release was
	// considered legal — but in practice that left buckets stuck
	// across a style cutover because Drain's old style-included WHERE
	// could no longer match them. The new invariant is "at most one
	// active bucket per (node, part)" regardless of style; DeactivateOtherStyles
	// (uop/capture.go:92) still deactivates other-style buckets on
	// capture to keep state coherent. Schema enforcement is strictly
	// stronger than the prior code-only enforcement.
	//
	// SQLite's CREATE UNIQUE INDEX IF NOT EXISTS matches on name only
	// — schema.Apply with the new DDL shape would skip recreating an
	// existing same-named index — so detect the old shape via the
	// presence of style_id in sqlite_master and DROP+CREATE to install
	// the narrower variant. Idempotent: on a DB already on the new
	// shape, the SELECT returns 0 rows and the block is a no-op.
	if hasLegacyLinesideStyleIndex(db) {
		db.Exec("DROP INDEX IF EXISTS idx_lineside_active_unique")
		db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_lineside_active_unique
			ON node_lineside_bucket(node_id, part_number)
			WHERE state = 'active'`)
	}

	// inventory_delta_seq PK extends from (scope_kind, scope_key) to
	// (scope_kind, scope_key, epoch). SQLite can't ALTER PRIMARY KEY in
	// place, so add the column and rebuild the table. Old rows land at
	// epoch=0 — which matches what Core's pre-existing dedup rows were
	// backfilled to in v22 — so a pre-migration Edge build's seq stream
	// continues coherently against the existing dedup baseline. The
	// next bin lifecycle event on Core bumps both sides to epoch>=1
	// and the new dedup row at the new epoch starts fresh.
	if hasLegacyInventoryDeltaSeqPK(db) {
		db.Exec(`CREATE TABLE inventory_delta_seq_new (
			scope_kind TEXT NOT NULL,
			scope_key  TEXT NOT NULL,
			epoch      INTEGER NOT NULL DEFAULT 0,
			next_seq   INTEGER NOT NULL DEFAULT 1,
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (scope_kind, scope_key, epoch)
		)`)
		db.Exec(`INSERT INTO inventory_delta_seq_new
			(scope_kind, scope_key, epoch, next_seq, updated_at)
			SELECT scope_kind, scope_key, 0, next_seq, updated_at
			FROM inventory_delta_seq`)
		db.Exec(`DROP TABLE inventory_delta_seq`)
		db.Exec(`ALTER TABLE inventory_delta_seq_new RENAME TO inventory_delta_seq`)
	}

	// v27: queue_reason on orders — mirrors Core's orders.queue_reason so
	// the operator HMI can show WHY an order is waiting instead of just "IN QUEUE".
	// Populated via the OrderUpdate push when Core's dispatcher leaves an order
	// queued after a scanner pass. Empty on non-queued orders; cleared implicitly
	// when the HMI renders non-queued statuses.
	db.Exec("ALTER TABLE orders ADD COLUMN queue_reason TEXT NOT NULL DEFAULT ''")

	return nil
}

// hasLegacyLinesideStyleIndex returns true if the lineside unique
// index still includes style_id in its definition. SQLite stores the
// full CREATE statement of every index in sqlite_master.sql, so a
// simple LIKE on style_id is enough to distinguish the old and new
// shapes — both reference part_number, but only the legacy shape
// references style_id.
func hasLegacyLinesideStyleIndex(db *DB) bool {
	var sql string
	err := db.QueryRow(`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type='index' AND name='idx_lineside_active_unique'`).Scan(&sql)
	if err != nil {
		return false
	}
	return strings.Contains(sql, "style_id")
}

// ── Table rebuild helpers (rename-rebuild pattern) ───────────────────

// migrateProcessColumns renames active_job_style_id → active_style_id
func (db *DB) migrateProcessColumns() error {
	has, err := schema.TableHasColumn(db.DB, "processes", "active_job_style_id")
	if err != nil || !has {
		// Already migrated or fresh DB — ensure new columns exist
		db.Exec("ALTER TABLE processes ADD COLUMN target_style_id INTEGER REFERENCES styles(id) ON DELETE SET NULL")
		db.Exec("ALTER TABLE processes ADD COLUMN production_state TEXT NOT NULL DEFAULT 'active_production'")
		return err
	}
	// Rebuild table with new column names
	_, err = db.Exec(`
ALTER TABLE processes RENAME TO processes_legacy;
CREATE TABLE processes (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL UNIQUE,
    description         TEXT NOT NULL DEFAULT '',
    active_style_id     INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    target_style_id     INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    production_state    TEXT NOT NULL DEFAULT 'active_production',
    created_at          TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO processes (id, name, description, active_style_id, target_style_id, production_state, created_at)
SELECT id, name, description, active_job_style_id,
    CASE WHEN EXISTS(SELECT 1 FROM pragma_table_info('processes_legacy') WHERE name='target_job_style_id')
         THEN target_job_style_id ELSE NULL END,
    COALESCE(CASE WHEN EXISTS(SELECT 1 FROM pragma_table_info('processes_legacy') WHERE name='production_state')
         THEN production_state ELSE NULL END, 'active_production'),
    created_at
FROM processes_legacy;
DROP TABLE processes_legacy;
`)
	return err
}

// migrateStyleColumns renames line_id → process_id
func (db *DB) migrateStyleColumns() error {
	hasLineID, err := schema.TableHasColumn(db.DB, "styles", "line_id")
	if err != nil {
		return err
	}
	if !hasLineID {
		// Fresh DB or already migrated — ensure process_id column
		db.Exec("ALTER TABLE styles ADD COLUMN process_id INTEGER REFERENCES processes(id) ON DELETE CASCADE")
		return nil
	}
	// Has line_id — rebuild.
	_, err = db.Exec(`
ALTER TABLE styles RENAME TO styles_legacy;
CREATE TABLE styles (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id  INTEGER REFERENCES processes(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, name)
);
INSERT INTO styles (id, process_id, name, description, created_at)
SELECT id, line_id, name, description, created_at
FROM styles_legacy;
DROP TABLE styles_legacy;
`)
	return err
}

// stripDeadStyleColumns removes unused is_default and active columns from styles.
func (db *DB) stripDeadStyleColumns() error {
	has, _ := schema.TableHasColumn(db.DB, "styles", "is_default")
	if !has {
		return nil
	}
	_, err := db.Exec(`
ALTER TABLE styles RENAME TO styles_strip;
CREATE TABLE styles (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id  INTEGER REFERENCES processes(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, name)
);
INSERT INTO styles (id, process_id, name, description, created_at)
SELECT id, process_id, name, description, created_at
FROM styles_strip;
DROP TABLE styles_strip;
`)
	return err
}

// migrateReportingPointColumns renames job_style_id → style_id
func (db *DB) migrateReportingPointColumns() error {
	has, err := schema.TableHasColumn(db.DB, "reporting_points", "job_style_id")
	if err != nil || !has {
		return err
	}
	_, err = db.Exec(`
ALTER TABLE reporting_points RENAME TO reporting_points_legacy;
CREATE TABLE reporting_points (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    style_id        INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    plc_name        TEXT NOT NULL,
    tag_name        TEXT NOT NULL,
    last_count      INTEGER NOT NULL DEFAULT 0,
    last_poll_at    TEXT,
    enabled         INTEGER NOT NULL DEFAULT 1,
    warlink_managed INTEGER NOT NULL DEFAULT 0,
    UNIQUE(plc_name, tag_name)
);
INSERT INTO reporting_points (id, style_id, plc_name, tag_name, last_count, last_poll_at, enabled, warlink_managed)
SELECT id, job_style_id, plc_name, tag_name, last_count, last_poll_at, enabled, COALESCE(warlink_managed, 0)
FROM reporting_points_legacy;
DROP TABLE reporting_points_legacy;
`)
	return err
}

// migrateHourlyCountColumns renames line_id → process_id, job_style_id → style_id
func (db *DB) migrateHourlyCountColumns() error {
	has, err := schema.TableHasColumn(db.DB, "hourly_counts", "line_id")
	if err != nil || !has {
		return err
	}
	_, err = db.Exec(`
ALTER TABLE hourly_counts RENAME TO hourly_counts_legacy;
CREATE TABLE hourly_counts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    count_date   TEXT NOT NULL,
    hour         INTEGER NOT NULL,
    delta        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (datetime('now')),
    UNIQUE(process_id, style_id, count_date, hour)
);
INSERT INTO hourly_counts (id, process_id, style_id, count_date, hour, delta, updated_at)
SELECT id, line_id, job_style_id, count_date, hour, delta, updated_at
FROM hourly_counts_legacy;
DROP TABLE hourly_counts_legacy;
`)
	return err
}

// migrateHourlyCountFKs adds foreign keys to hourly_counts if missing.
func (db *DB) migrateHourlyCountFKs() error {
	var tableSql string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='hourly_counts'`).Scan(&tableSql)
	if err != nil {
		return nil // table doesn't exist yet, schema constant will create it
	}
	if strings.Contains(tableSql, "REFERENCES") {
		return nil // already has FKs
	}
	// Prune orphans before adding FK constraints
	db.Exec(`DELETE FROM hourly_counts WHERE process_id NOT IN (SELECT id FROM processes)`)
	db.Exec(`DELETE FROM hourly_counts WHERE style_id NOT IN (SELECT id FROM styles)`)
	_, err = db.Exec(`
ALTER TABLE hourly_counts RENAME TO hourly_counts_nofk;
CREATE TABLE hourly_counts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    count_date   TEXT NOT NULL,
    hour         INTEGER NOT NULL,
    delta        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (datetime('now')),
    UNIQUE(process_id, style_id, count_date, hour)
);
INSERT INTO hourly_counts (id, process_id, style_id, count_date, hour, delta, updated_at)
SELECT id, process_id, style_id, count_date, hour, delta, updated_at
FROM hourly_counts_nofk;
DROP TABLE hourly_counts_nofk;
`)
	return err
}

// migrateOperatorStationColumns removes parent_station_id and expected_client_type
func (db *DB) migrateOperatorStationColumns() error {
	hasParent, err := schema.TableHasColumn(db.DB, "operator_stations", "parent_station_id")
	if err != nil {
		return err
	}
	hasExpected, _ := schema.TableHasColumn(db.DB, "operator_stations", "expected_client_type")
	if !hasParent && !hasExpected {
		// Add columns that may be missing on legacy DBs
		db.Exec("ALTER TABLE operator_stations ADD COLUMN note TEXT NOT NULL DEFAULT ''")
		db.Exec("ALTER TABLE operator_stations ADD COLUMN controller_node_id TEXT NOT NULL DEFAULT ''")
		return nil
	}
	_, err = db.Exec(`
ALTER TABLE operator_stations RENAME TO operator_stations_legacy;
CREATE TABLE operator_stations (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id         INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    code               TEXT NOT NULL,
    name               TEXT NOT NULL,
    note               TEXT NOT NULL DEFAULT '',
    area_label         TEXT NOT NULL DEFAULT '',
    sequence           INTEGER NOT NULL DEFAULT 0,
    controller_node_id TEXT NOT NULL DEFAULT '',
    device_mode        TEXT NOT NULL DEFAULT 'touch_hmi',
    enabled            INTEGER NOT NULL DEFAULT 1,
    health_status      TEXT NOT NULL DEFAULT 'offline',
    last_seen_at       TEXT,
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, code)
);
INSERT INTO operator_stations (
    id, process_id, code, name, note, area_label, sequence, controller_node_id,
    device_mode, enabled, health_status, last_seen_at, created_at, updated_at
)
SELECT
    id, process_id, code, name, COALESCE(note, ''), COALESCE(area_label, ''), COALESCE(sequence, 0), COALESCE(controller_node_id, ''),
    COALESCE(device_mode, 'touch_hmi'), COALESCE(enabled, 1), COALESCE(health_status, 'offline'), last_seen_at, created_at, updated_at
FROM operator_stations_legacy;
DROP TABLE operator_stations_legacy;
`)
	return err
}

// ── Process node ownership migration ────────────────────────────────

// migrateProcessNodeOwnership migrates from op_station_nodes + junction table to inline operator_station_id
func (db *DB) migrateProcessNodeOwnership() error {
	// Phase 1: migrate from old op_station_nodes table if it exists
	opStationNodesExist, err := schema.TableExists(db.DB, "op_station_nodes")
	if err != nil {
		return err
	}
	if opStationNodesExist {
		if err := db.rebuildFromOpStationNodes(); err != nil {
			return err
		}
	}

	// Phase 2: migrate from junction table to inline FK (if junction table exists)
	junctionExists, err := schema.TableExists(db.DB, "operator_station_process_nodes")
	if err != nil {
		return err
	}
	if junctionExists {
		hasInlineFK, err := schema.TableHasColumn(db.DB, "process_nodes", "operator_station_id")
		if err != nil {
			return err
		}
		if !hasInlineFK {
			// Need to rebuild process_nodes with inline FK (simplified schema)
			_, err = db.Exec(`
ALTER TABLE process_nodes RENAME TO process_nodes_legacy;
CREATE TABLE process_nodes (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id          INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    operator_station_id INTEGER REFERENCES operator_stations(id) ON DELETE SET NULL,
    core_node_name      TEXT NOT NULL DEFAULT '',
    code                TEXT NOT NULL,
    name                TEXT NOT NULL,
    sequence            INTEGER NOT NULL DEFAULT 0,
    enabled             INTEGER NOT NULL DEFAULT 1,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, code)
);
INSERT INTO process_nodes (
    id, process_id, operator_station_id, core_node_name, code, name, sequence, enabled, created_at, updated_at
)
SELECT
    n.id, n.process_id, d.operator_station_id, COALESCE(n.core_node_name, ''), n.code, n.name, n.sequence, n.enabled, n.created_at, n.updated_at
FROM process_nodes_legacy n
LEFT JOIN operator_station_process_nodes d ON d.process_node_id = n.id;
DROP TABLE process_nodes_legacy;
`)
			if err != nil {
				return err
			}
		}

		// Drop junction table
		db.Exec("DROP TABLE IF EXISTS operator_station_process_nodes")
	}

	// Strip old routing/allows columns from process_nodes if they exist
	if err := db.stripLegacyProcessNodeColumns(); err != nil {
		return err
	}

	// Also rebuild related tables that had op_node_id references
	if err := db.rebuildLegacyAssignments(); err != nil {
		return err
	}
	if err := db.rebuildLegacyRuntimeStates(); err != nil {
		return err
	}
	if err := db.rebuildLegacyOrders(); err != nil {
		return err
	}
	if err := db.rebuildLegacyChangeoverNodeTasks(); err != nil {
		return err
	}

	// skip_note carries the operator-facing message when a linked complex
	// order reached terminal "skipped" (e.g. evac for a bin that was
	// removed externally before dispatch). Surfaced as a chip on the node
	// tile via StationNodeView.SkipNote. Idempotent ADD COLUMN; the
	// rebuildLegacyChangeoverNodeTasks() path above already creates the
	// column on fresh schemas.
	db.Exec("ALTER TABLE changeover_node_tasks ADD COLUMN skip_note TEXT NOT NULL DEFAULT ''")

	return nil
}

// renameRemainingUOPCached covers an intermediate schema state where
// effective_style_id was already gone (so stripLegacyRuntimeStateColumns
// skips its rebuild) but the column was still named remaining_uop.
// Idempotent: ALTER ADD COLUMN is silently ignored if the new column
// already exists; the backfill + DROP only runs when the legacy column
// is still present, so subsequent startups are no-ops.
func (db *DB) renameRemainingUOPCached() error {
	hasLegacy, _ := schema.TableHasColumn(db.DB, "process_node_runtime_states", "remaining_uop")
	if !hasLegacy {
		return nil
	}
	db.Exec("ALTER TABLE process_node_runtime_states ADD COLUMN remaining_uop_cached INTEGER NOT NULL DEFAULT 0")
	if _, err := db.Exec("UPDATE process_node_runtime_states SET remaining_uop_cached = remaining_uop"); err != nil {
		return err
	}
	_, err := db.Exec("ALTER TABLE process_node_runtime_states DROP COLUMN remaining_uop")
	return err
}

func (db *DB) stripLegacyRuntimeStateColumns() error {
	hasOldCol, _ := schema.TableHasColumn(db.DB, "process_node_runtime_states", "effective_style_id")
	if !hasOldCol {
		return nil
	}
	_, err := db.Exec(`
ALTER TABLE process_node_runtime_states RENAME TO process_node_runtime_states_old;
CREATE TABLE process_node_runtime_states (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    process_node_id      INTEGER NOT NULL UNIQUE REFERENCES process_nodes(id) ON DELETE CASCADE,
    active_claim_id      INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
    remaining_uop_cached INTEGER NOT NULL DEFAULT 0,
    active_order_id      INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    staged_order_id      INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    updated_at           TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO process_node_runtime_states (id, process_node_id, remaining_uop_cached, active_order_id, staged_order_id, updated_at)
SELECT id, process_node_id, remaining_uop, active_order_id, staged_order_id, updated_at
FROM process_node_runtime_states_old;
DROP TABLE process_node_runtime_states_old;
`)
	return err
}

func (db *DB) stripLegacyProcessNodeColumns() error {
	hasPositionType, _ := schema.TableHasColumn(db.DB, "process_nodes", "position_type")
	if !hasPositionType {
		return nil // already stripped
	}
	_, err := db.Exec(`
ALTER TABLE process_nodes RENAME TO process_nodes_old;
CREATE TABLE process_nodes (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id          INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    operator_station_id INTEGER REFERENCES operator_stations(id) ON DELETE SET NULL,
    core_node_name      TEXT NOT NULL DEFAULT '',
    code                TEXT NOT NULL,
    name                TEXT NOT NULL,
    sequence            INTEGER NOT NULL DEFAULT 0,
    enabled             INTEGER NOT NULL DEFAULT 1,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, code)
);
INSERT INTO process_nodes (id, process_id, operator_station_id, core_node_name, code, name, sequence, enabled, created_at, updated_at)
SELECT id, process_id, operator_station_id, COALESCE(core_node_name, ''), code, name, sequence, enabled, created_at, updated_at
FROM process_nodes_old;
DROP TABLE process_nodes_old;
`)
	return err
}

func (db *DB) rebuildFromOpStationNodes() error {
	// Check if process_nodes already has data
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM process_nodes`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		db.Exec("DROP TABLE IF EXISTS op_station_nodes")
		return nil
	}

	hasCoreNodeName, _ := schema.TableHasColumn(db.DB, "op_station_nodes", "core_node_name")
	coreNodeExpr := "''"
	if hasCoreNodeName {
		coreNodeExpr = "COALESCE(n.core_node_name, '')"
	}

	// Migrate directly to inline operator_station_id model (simplified schema)
	_, err := db.Exec(`
INSERT INTO process_nodes (
    id, process_id, operator_station_id, core_node_name, code, name, sequence, enabled, created_at, updated_at
)
SELECT
    n.id, s.process_id, n.operator_station_id, ` + coreNodeExpr + `, n.code, n.name, COALESCE(n.sequence, 0),
    COALESCE(n.enabled, 1), n.created_at, n.updated_at
FROM op_station_nodes n
JOIN operator_stations s ON s.id = n.operator_station_id;
`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`DROP TABLE IF EXISTS op_station_nodes`)
	return err
}

func (db *DB) rebuildLegacyAssignments() error {
	// Legacy assignment tables are no longer needed (replaced by style_node_claims).
	// Just drop them if they exist.
	for _, name := range []string{"op_node_style_assignments_legacy", "op_node_style_assignments"} {
		db.Exec(`DROP TABLE IF EXISTS ` + name)
	}
	return nil
}

func (db *DB) rebuildLegacyRuntimeStates() error {
	for _, name := range []string{"op_node_runtime_states_legacy", "op_node_runtime_states"} {
		has, err := schema.TableHasColumn(db.DB, name, "op_node_id")
		if err != nil || !has {
			continue
		}
		_, err = db.Exec(`INSERT OR IGNORE INTO process_node_runtime_states (
    id, process_node_id, remaining_uop_cached,
    active_order_id, staged_order_id, updated_at
)
SELECT id, op_node_id, remaining_uop,
    active_order_id, staged_order_id, updated_at
FROM ` + name)
		if err != nil {
			return err
		}
		db.Exec(`DROP TABLE IF EXISTS ` + name)
		return nil
	}
	return nil
}

func (db *DB) rebuildLegacyOrders() error {
	has, err := schema.TableHasColumn(db.DB, "orders", "op_node_id")
	if err != nil || !has {
		return err
	}
	_, err = db.Exec(`
ALTER TABLE orders RENAME TO orders_legacy;
CREATE TABLE orders (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    uuid            TEXT NOT NULL UNIQUE,
    order_type      TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    process_node_id INTEGER REFERENCES process_nodes(id) ON DELETE SET NULL,
    retrieve_empty  INTEGER NOT NULL DEFAULT 1,
    quantity        INTEGER NOT NULL DEFAULT 0,
    delivery_node   TEXT NOT NULL DEFAULT '',
    staging_node    TEXT NOT NULL DEFAULT '',
    source_node     TEXT NOT NULL DEFAULT '',
    load_type       TEXT NOT NULL DEFAULT '',
    waybill_id      TEXT,
    external_ref    TEXT,
    final_count     INTEGER,
    count_confirmed INTEGER NOT NULL DEFAULT 0,
    eta             TEXT,
    auto_confirm    INTEGER NOT NULL DEFAULT 0,
    steps_json      TEXT NOT NULL DEFAULT '',
    staged_expire_at TEXT,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO orders (
    id, uuid, order_type, status, process_node_id, retrieve_empty, quantity, delivery_node, staging_node, source_node,
    load_type, waybill_id, external_ref, final_count, count_confirmed, eta, auto_confirm, created_at, updated_at,
    steps_json, staged_expire_at
)
SELECT
    id, uuid, order_type, status, op_node_id, retrieve_empty, quantity, delivery_node, staging_node, pickup_node,
    load_type, waybill_id, external_ref, final_count, count_confirmed, eta, auto_confirm, created_at, updated_at,
    COALESCE(steps_json, ''), staged_expire_at
FROM orders_legacy;
DROP TABLE orders_legacy;
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_uuid ON orders(uuid);
CREATE INDEX IF NOT EXISTS idx_orders_process_node_id ON orders(process_node_id);
`)
	return err
}

func (db *DB) rebuildLegacyChangeoverNodeTasks() error {
	// Drop legacy changeover node task tables. Old changeover data is not
	// migrated — the schema has changed fundamentally (claims vs assignments).
	db.Exec("DROP TABLE IF EXISTS changeover_node_tasks_legacy")

	// If the current table has the old column layout, rebuild it
	hasOldCol, _ := schema.TableHasColumn(db.DB, "changeover_node_tasks", "changeover_station_task_id")
	if !hasOldCol {
		hasOldCol, _ = schema.TableHasColumn(db.DB, "changeover_node_tasks", "from_assignment_id")
	}
	if hasOldCol {
		_, err := db.Exec(`
DROP TABLE IF EXISTS changeover_node_tasks;
CREATE TABLE IF NOT EXISTS changeover_node_tasks (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    process_changeover_id      INTEGER NOT NULL REFERENCES process_changeovers(id) ON DELETE CASCADE,
    process_node_id            INTEGER NOT NULL REFERENCES process_nodes(id) ON DELETE CASCADE,
    from_claim_id              INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
    to_claim_id                INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
    situation                  TEXT NOT NULL DEFAULT 'unchanged',
    state                      TEXT NOT NULL DEFAULT 'pending',
    next_material_order_id     INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    old_material_release_order_id INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    skip_note                  TEXT NOT NULL DEFAULT '',
    updated_at                 TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_changeover_id, process_node_id)
);
`)
		return err
	}
	return nil
}

// collapseDuplicateProcessNodes merges process_nodes rows that share a
// (process_id, core_node_name) down to one, then enforces that shape with a
// UNIQUE index so they can't come back.
//
// How the duplicates happened: SetNodes decided "reuse or create?" from the
// STATION-local node set, so re-adding a Core node to a station that didn't
// already own a row for it minted a fresh row instead of adopting the existing
// one. GenerateUniqueCode then suffixed the code (pln-01, pln-01-2, pln-01-3) to
// satisfy the only constraint the table had — UNIQUE(process_id, code) — while
// core_node_name was left free to duplicate. HK carried three PLN_01 rows.
//
// Why it matters: findActiveClaim resolves a claim by core_node_name, not by node
// id, so EVERY duplicate matched the same active claim and handleCounterDelta
// (which iterates all nodes in the process) applied each PLC tick to all of them.
// One press stroke counted three times: once on the live row, once against a bin
// the orphan still pointed at, and once into a bin-less row where it piled up in
// pending_uop_delta forever (HK: 28,670).
//
// Survivor: the station-bound row wins (it is the one the HMI reads); then the
// freshest runtime; then the lowest id. Referrers are repointed at it. The dead
// rows' runtime state is DISCARDED, not replayed — a duplicate's counts are
// phantom by construction (they double-counted a stroke that the survivor already
// booked), and a bin-less row's pending_uop_delta never had a bin to replay onto.
// Anything discarded is logged rather than dropped silently.
//
// Idempotent: a no-op on a database with no duplicates.
func (db *DB) collapseDuplicateProcessNodes() error {
	exists, err := schema.TableExists(db.DB, "process_nodes")
	if err != nil || !exists {
		return err
	}
	if hasCol, cErr := schema.TableHasColumn(db.DB, "process_nodes", "core_node_name"); cErr != nil || !hasCol {
		return cErr
	}

	// core_node_name is NOT NULL DEFAULT '' and nothing validates it — CreateNode
	// only trims, and apiCreateProcessNode decodes straight into it — so a node can
	// legitimately exist UNBOUND (no Core node yet). Two unbound rows are NOT
	// duplicates of each other: they are distinct nodes that merely share the empty
	// string. Grouping them would delete real nodes, discard their runtime and
	// repoint their orders onto an unrelated survivor. Exclude them here, and make
	// the index below partial on the same predicate so unbound nodes stay possible.
	rows, err := db.Query(`
		SELECT process_id, core_node_name FROM process_nodes
		WHERE core_node_name <> ''
		GROUP BY process_id, core_node_name HAVING COUNT(*) > 1`)
	if err != nil {
		return err
	}
	var groups []dupGroup
	for rows.Next() {
		var g dupGroup
		if err := rows.Scan(&g.processID, &g.coreNodeName); err != nil {
			rows.Close()
			return err
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, g := range groups {
		if err := db.collapseProcessNodeGroup(g); err != nil {
			return err
		}
	}

	// The constraint that should have existed all along. PARTIAL — unbound nodes
	// (core_node_name = '') are exempt, because they are not duplicates of each
	// other and a process may hold several. Created here rather than in the
	// canonical DDL because it cannot be built until the collapse above has run:
	// schema.Apply would fail on any database that still has duplicates.
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_process_nodes_process_core_name
		ON process_nodes(process_id, core_node_name)
		WHERE core_node_name <> ''`); err != nil {
		return fmt.Errorf("enforce UNIQUE(process_id, core_node_name): %w", err)
	}
	return nil
}

// dupGroup is one (process, core node) that has more than one process_nodes row.
type dupGroup struct {
	processID    int64
	coreNodeName string
}

// collapseProcessNodeGroup merges one duplicate group down to a single row,
// ATOMICALLY. Every repoint and delete for the group is one transaction: this
// runs unattended at edge startup against a live plant database, and a collapse
// that dies halfway — orders repointed, node still present, or worse — is a shape
// nobody has ever reasoned about. Either the whole group collapses or the
// database is exactly as it was and the next startup tries again.
//
// Survivor: the station-bound row wins (it is the one the HMI reads); then the
// freshest runtime; then the lowest id.
//
// What is destroyed is announced, and only after the commit that destroyed it —
// logging a discard that then rolled back would be a lie in the one record anyone
// has of it.
func (db *DB) collapseProcessNodeGroup(g dupGroup) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("collapse %s: begin: %w", g.coreNodeName, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit has succeeded

	var survivor int64
	if err := tx.QueryRow(`
		SELECT pn.id FROM process_nodes pn
		LEFT JOIN process_node_runtime_states r ON r.process_node_id = pn.id
		WHERE pn.process_id = ? AND pn.core_node_name = ?
		ORDER BY (pn.operator_station_id IS NOT NULL) DESC,
		         COALESCE(r.updated_at, '') DESC,
		         pn.id ASC
		LIMIT 1`, g.processID, g.coreNodeName).Scan(&survivor); err != nil {
		return fmt.Errorf("collapse %s: pick survivor: %w", g.coreNodeName, err)
	}

	dead, err := scanIDs(tx.Query(`
		SELECT id FROM process_nodes
		WHERE process_id = ? AND core_node_name = ? AND id <> ?`,
		g.processID, g.coreNodeName, survivor))
	if err != nil {
		return fmt.Errorf("collapse %s: list duplicates: %w", g.coreNodeName, err)
	}

	// Accumulated here, printed after the commit.
	var discards []string

	for _, d := range dead {
		var pending, uop int
		var binID sql.NullInt64
		_ = tx.QueryRow(`SELECT pending_uop_delta, remaining_uop_cached, active_bin_id
			FROM process_node_runtime_states WHERE process_node_id = ?`, d).Scan(&pending, &uop, &binID)
		discards = append(discards, fmt.Sprintf(
			"migrate: collapsed duplicate process_node %d (%s, process %d) into %d — discarded runtime: pending_uop_delta=%d remaining_uop_cached=%d active_bin_id=%v (phantom: these ticks were double-counted onto the survivor)",
			d, g.coreNodeName, g.processID, survivor, pending, uop, binID))

		// A dead row that is itself bound to a station means the node was SHARED by
		// two live stations, and one of them is about to lose it from its board.
		// That config was already broken — both rows drew every PLC tick — so
		// collapsing is right, but it is a visible change to an operator's screen
		// and must not happen quietly.
		var deadStation sql.NullInt64
		_ = tx.QueryRow(`SELECT operator_station_id FROM process_nodes WHERE id = ?`, d).Scan(&deadStation)
		if deadStation.Valid {
			discards = append(discards, fmt.Sprintf(
				"migrate: WARNING — %s was shared by operator stations %d and (survivor) — station %d LOSES this node from its board. The shared config was double-counting its PLC ticks; re-add the node to that station if it is still wanted there.",
				g.coreNodeName, deadStation.Int64, deadStation.Int64))
		}

		// orders.process_node_id — no uniqueness, straight repoint.
		if _, err := tx.Exec(`UPDATE orders SET process_node_id = ? WHERE process_node_id = ?`, survivor, d); err != nil {
			return fmt.Errorf("collapse %s: repoint orders: %w", g.coreNodeName, err)
		}

		// changeover_node_tasks — UNIQUE(process_changeover_id, process_node_id).
		// Move only what won't collide; the survivor's row wins the rest.
		if _, err := tx.Exec(`
			UPDATE changeover_node_tasks SET process_node_id = ?
			WHERE process_node_id = ?
			  AND NOT EXISTS (SELECT 1 FROM changeover_node_tasks s
			                  WHERE s.process_node_id = ?
			                    AND s.process_changeover_id = changeover_node_tasks.process_changeover_id)`,
			survivor, d, survivor); err != nil {
			return fmt.Errorf("collapse %s: repoint changeover tasks: %w", g.coreNodeName, err)
		}
		// Whatever is still pointing at the dead row collided with the survivor's
		// own task and is about to be dropped. Name it — an operator's changeover
		// state vanishing without a line in the log is not something you can debug
		// after the fact.
		lost, err := scanTaskLosses(tx.Query(`SELECT id, process_changeover_id, state
			FROM changeover_node_tasks WHERE process_node_id = ?`, d))
		if err != nil {
			return fmt.Errorf("collapse %s: read colliding changeover tasks: %w", g.coreNodeName, err)
		}
		for _, l := range lost {
			discards = append(discards, fmt.Sprintf(
				"migrate: dropped changeover_node_task %d (changeover %d, state=%q) from duplicate node %d (%s) — the survivor %d already holds a task for that changeover",
				l.id, l.changeoverID, l.state, d, g.coreNodeName, survivor))
		}
		if _, err := tx.Exec(`DELETE FROM changeover_node_tasks WHERE process_node_id = ?`, d); err != nil {
			return fmt.Errorf("collapse %s: delete changeover tasks: %w", g.coreNodeName, err)
		}

		// node_lineside_bucket — UNIQUE(node_id, part_number) WHERE state='active'.
		//
		// The guard tests the MOVING row's state as well as the survivor's. The
		// index is partial on state='active', so an INACTIVE bucket cannot collide
		// with anything and must always migrate. Without that clause it was matched
		// by a survivor's active row of the same part number, refused the move, and
		// then deleted — throwing away closed-out part counts that were never in
		// anyone's way.
		if _, err := tx.Exec(`
			UPDATE node_lineside_bucket SET node_id = ?
			WHERE node_id = ?
			  AND NOT EXISTS (SELECT 1 FROM node_lineside_bucket s
			                  WHERE s.node_id = ?
			                    AND s.part_number = node_lineside_bucket.part_number
			                    AND s.state = 'active'
			                    AND node_lineside_bucket.state = 'active')`,
			survivor, d, survivor); err != nil {
			return fmt.Errorf("collapse %s: repoint lineside buckets: %w", g.coreNodeName, err)
		}
		// What survives that UPDATE is an ACTIVE bucket the survivor already has a
		// row for. These carry operator-captured part quantities — say what is lost.
		buckets, err := scanBucketLosses(tx.Query(`SELECT id, part_number, qty, state
			FROM node_lineside_bucket WHERE node_id = ?`, d))
		if err != nil {
			return fmt.Errorf("collapse %s: read colliding lineside buckets: %w", g.coreNodeName, err)
		}
		for _, b := range buckets {
			discards = append(discards, fmt.Sprintf(
				"migrate: dropped lineside bucket %d (part=%s qty=%d state=%q) from duplicate node %d (%s) — the survivor %d already holds an active bucket for that part; re-count it lineside if the quantity was real",
				b.id, b.part, b.qty, b.state, d, g.coreNodeName, survivor))
		}
		if _, err := tx.Exec(`DELETE FROM node_lineside_bucket WHERE node_id = ?`, d); err != nil {
			return fmt.Errorf("collapse %s: delete lineside buckets: %w", g.coreNodeName, err)
		}

		// Runtime is UNIQUE per node — the survivor already has the real one.
		if _, err := tx.Exec(`DELETE FROM process_node_runtime_states WHERE process_node_id = ?`, d); err != nil {
			return fmt.Errorf("collapse %s: delete runtime: %w", g.coreNodeName, err)
		}
		if _, err := tx.Exec(`DELETE FROM process_nodes WHERE id = ?`, d); err != nil {
			return fmt.Errorf("collapse %s: delete duplicate %d: %w", g.coreNodeName, d, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("collapse %s: commit: %w", g.coreNodeName, err)
	}
	for _, line := range discards {
		log.Print(line)
	}
	return nil
}

type taskLoss struct {
	id           int64
	changeoverID int64
	state        string
}

type bucketLoss struct {
	id    int64
	part  string
	qty   int
	state string
}

func scanIDs(rows *sql.Rows, qErr error) ([]int64, error) {
	if qErr != nil {
		return nil, qErr
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func scanTaskLosses(rows *sql.Rows, qErr error) ([]taskLoss, error) {
	if qErr != nil {
		return nil, qErr
	}
	defer rows.Close()
	var out []taskLoss
	for rows.Next() {
		var l taskLoss
		if err := rows.Scan(&l.id, &l.changeoverID, &l.state); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func scanBucketLosses(rows *sql.Rows, qErr error) ([]bucketLoss, error) {
	if qErr != nil {
		return nil, qErr
	}
	defer rows.Close()
	var out []bucketLoss
	for rows.Next() {
		var b bucketLoss
		if err := rows.Scan(&b.id, &b.part, &b.qty, &b.state); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ── Compatibility wrappers (kept for migration_test.go) ─────────────

// tableHasColumn delegates to schema.TableHasColumn so existing
// migration_test.go call sites compile unchanged. Phase 6.4 may
// migrate the test directly to the schema package and drop this
// wrapper.
func (db *DB) tableHasColumn(tableName, columnName string) (bool, error) {
	return schema.TableHasColumn(db.DB, tableName, columnName)
}
