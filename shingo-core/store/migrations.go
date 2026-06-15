package store

import (
	"database/sql"
	"fmt"
	"log"

	"shingocore/store/schema"
)

// migrateRenames idempotently renames old columns to vendor-neutral names.
// These run BEFORE the baseline DDL because CREATE TABLE IF NOT EXISTS would
// skip tables whose only divergence from current is column names — leaving the
// database wedged on the old schema with no way forward.
//
// Pre-baseline so we run against the connection pool, not a transaction.
// Each rename is its own transaction at the DDL level; partial failures
// are caught by the explicit error return.
func (db *DB) migrateRenames() error {
	renames := []struct{ table, oldCol, newCol string }{
		{"orders", "rds_order_id", "vendor_order_id"},
		{"orders", "rds_state", "vendor_state"},
		{"orders", "client_id", "station_id"},
		{"orders", "pickup_node", "source_node"},
		{"mission_telemetry", "pickup_node", "source_node"},
		{"outbox", "event_type", "msg_type"},
		{"outbox", "client_id", "station_id"},
	}
	for _, r := range renames {
		if schema.ColumnExists(db.DB, r.table, r.oldCol) {
			_, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s RENAME COLUMN %s TO %s`, r.table, r.oldCol, r.newCol))
			if err != nil {
				return fmt.Errorf("rename %s.%s: %w", r.table, r.oldCol, err)
			}
		}
	}
	db.Exec(`DROP INDEX IF EXISTS idx_orders_rds`)
	db.Exec("UPDATE orders SET status='confirmed' WHERE status='completed'")
	return nil
}

// migrate runs column renames (for ancient databases), pre-baseline column
// adds for tables the baseline DDL indexes by a column that existed on no
// prior schema, the baseline DDL via the schema sub-package, then versioned
// migrations. Order matters: renames and column adds fix tables that the
// baseline CREATE ... IF NOT EXISTS would otherwise skip — without them,
// a CREATE INDEX in the baseline DDL that references a not-yet-added column
// fails before any versioned migration gets a chance to run.
func (db *DB) migrate() error {
	if err := db.migrateRenames(); err != nil {
		return fmt.Errorf("migrate renames: %w", err)
	}
	if err := db.migrateAddBaselineColumns(); err != nil {
		return fmt.Errorf("migrate add baseline columns: %w", err)
	}
	if err := schema.Apply(db.DB); err != nil {
		return err
	}
	return db.runVersionedMigrations()
}

// migrateAddBaselineColumns idempotently adds columns the baseline DDL
// assumes-present (e.g. via a CREATE INDEX on the column) but which are
// not added by any versioned migration. Without this step, a DB created
// before the column landed in postgres_ddl.go's CREATE TABLE hits
// "column does not exist" inside schema.Apply, ahead of versioned
// migrations. Pre-baseline sibling of migrateRenames; same rationale.
//
// Pair with the schema-constant rule's reverse direction: whenever a
// column lands in postgres_ddl.go's CREATE TABLE for a table that
// already exists in production DBs, append it here. A pre-baseline ADD
// COLUMN IF NOT EXISTS is the minimum to keep old DBs starting.
func (db *DB) migrateAddBaselineColumns() error {
	adds := []struct{ table, column, ddl string }{
		{"bins", "payload_code", `ALTER TABLE bins ADD COLUMN IF NOT EXISTS payload_code TEXT NOT NULL DEFAULT ''`},
		{"cms_transactions", "payload_code", `ALTER TABLE cms_transactions ADD COLUMN IF NOT EXISTS payload_code TEXT NOT NULL DEFAULT ''`},
		{"demand_registry", "payload_code", `ALTER TABLE demand_registry ADD COLUMN IF NOT EXISTS payload_code TEXT NOT NULL DEFAULT ''`},
		{"lineside_buckets", "payload_code", `ALTER TABLE lineside_buckets ADD COLUMN IF NOT EXISTS payload_code TEXT NOT NULL DEFAULT ''`},
		// v21 adds core_node_name to lineside_buckets and drops node_id,
		// but the baseline DDL's CREATE INDEX idx_lineside_buckets_node_style
		// references core_node_name and runs ahead of versioned migrations.
		// Pre-baseline-add unblocks schema.Apply on plant DBs that still
		// carry the pre-v21 shape (Springfield, May 2026).
		{"lineside_buckets", "core_node_name", `ALTER TABLE lineside_buckets ADD COLUMN IF NOT EXISTS core_node_name TEXT NOT NULL DEFAULT ''`},
	}
	for _, a := range adds {
		if !schema.TableExists(db.DB, a.table) {
			continue
		}
		if _, err := db.Exec(a.ddl); err != nil {
			return fmt.Errorf("add %s.%s: %w", a.table, a.column, err)
		}
	}
	return nil
}

// migration is one numbered, tracked schema change.
//
// fn is the apply function — runs inside a per-version transaction
// alongside the schema_migrations row insert (see runOneMigration).
//
// verify is the post-condition check — given a Querier, returns true
// iff the schema state the migration is supposed to produce is
// actually present. Run on startup BEFORE the schema_migrations gate:
// if a row says "applied" but verify returns false, the runner deletes
// the row and re-applies the migration. Catches:
//
//   - Prior incomplete deploys that recorded the version row but
//     didn't commit DDL (the ALN_001-class scenario the transactional
//     wrap above prevents going forward but can't retroactively undo).
//   - Operator-induced drift: someone DROPped a column, or restored
//     a backup that predates the migration, leaving schema_migrations
//     ahead of actual schema.
//
// verify may be nil — for migrations whose post-condition is data-
// shaped (telemetry backfill) or trivial-or-noisy (boolean type
// conversions, drops). Nil verify means "trust schema_migrations" —
// same behavior as pre-self-heal.
//
// Implementation note: verify must be cheap. It runs on every Core
// startup for every applied migration. A single information_schema
// query is fine; a full table scan is not.
type migration struct {
	version int
	name    string
	fn      func(tx *sql.Tx) error
	verify  func(q schema.Querier) bool
}

// runVersionedMigrations runs numbered migrations that are tracked in a
// schema_migrations table.
//
// Two correctness layers:
//
//  1. **Transactional invariant** (runOneMigration): each migration's
//     DDL/DML AND the schema_migrations row insert run inside the same
//     transaction. Either both commit or neither does. Closes the
//     "DDL committed but version row missing" and "version row
//     committed but DDL silently no-op'd" failure modes.
//
//  2. **Self-heal-on-startup**: for migrations with a non-nil verify,
//     check the post-condition before trusting the schema_migrations
//     gate. If the row says "applied" but verify reports the state is
//     not present, delete the row and re-apply. Recovers plant DBs
//     from prior-bug damage and from operator drift without needing
//     manual SQL.
//
// Migrations also use `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` style
// so PostgreSQL itself enforces apply-once idempotency, not a Go-side
// schema.ColumnExists check that can lie under connection-pool /
// search_path edge cases.
// latestMigrationVersion is the highest migration version, captured from the
// migration list when it is built in runVersionedMigrations.
var latestMigrationVersion int

// LatestMigrationVersion returns the highest schema migration version this
// build defines. It is derived from the migration list (not a hand-maintained
// constant), so it can never drift from the migrations themselves. Populated
// when migrations are built/run; callers that compare against a live DB run
// migrations first.
func LatestMigrationVersion() int { return latestMigrationVersion }

func (db *DB) runVersionedMigrations() error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	migrations := []migration{
		// v1–v7: legacy migrations. verify=nil — these are old, the
		// schema they install is varied and partially data-shaped, and
		// any drift is operator-driven on ancient DBs nobody has
		// anymore. If a plant ever proves otherwise, add verify here.
		{1, "convert boolean columns to native BOOLEAN", v1BooleanColumns, nil},
		{2, "add depth column to nodes", v2DepthColumn, nil},
		{3, "drop dead columns", v3DropDeadColumns, nil},
		{4, "drop vestigial payload_id from orders", v4DropOrderPayloadID, nil},
		{5, "backfill mission telemetry for completed orders", v5MissionTelemetryBackfill, nil},
		{6, "consolidate legacy migrations", v6LegacyConsolidation, nil},
		{7, "drop vestigial default_manifest_json from payloads", v7DropDefaultManifestJSON, nil},

		// v8+: simple column-adding migrations. Verify is a single
		// information_schema query — cheap and reliable.
		{8, "add payload_code column to orders", v8OrderPayloadCode,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "orders", "payload_code") }},
		{9, "create order_bins junction table for multi-bin complex orders", v9OrderBins,
			func(q schema.Querier) bool { return schema.TableExists(q, "order_bins") }},
		{10, "add wait_index column to orders for multi-wait complex orders", v10OrderWaitIndex,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "orders", "wait_index") }},

		// v11–v13: FK fixes. Verify would inspect
		// information_schema.referential_constraints which is fiddly
		// and the failure mode is rare (a wrong FK on a fresh DB).
		// Leave nil — if a plant hits it, write the verify then.
		{11, "fix payload_bin_types FK to reference payloads instead of blueprints", v11FixPayloadBinTypesFK, nil},
		{12, "fix payload_manifest FK to reference payloads instead of blueprints", v12FixPayloadManifestFK, nil},
		{13, "fix node_payloads FK to reference payloads instead of blueprints", v13FixNodePayloadsFK, nil},

		// v14+: bin-transit-state migrations. These ARE the ones the
		// plant ALN_001-class deploy bug damaged, so verify is
		// non-negotiable here.
		{14, "add process_node column to orders", v14OrderProcessNode,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "orders", "process_node") }},
		{15, "add bin transit synthetic node and bins.anomaly_at", v15BinTransitState,
			verifyV15BinTransitState},
		{16, "add queue_reason column to orders", v16OrderQueueReason,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "orders", "queue_reason") }},

		// v17: UOP bin-as-truth refactor — audit log + delta apply
		// infrastructure, in final shape. Pre-production rollout
		// collapsed the staged Phases 0–4 sub-migrations into a
		// single migration once the design stabilized; the staged
		// versions never ran against a production DB.
		//
		// Net effect of v17 = v17a + v18 + v20 (auth-only) + v21
		// from the original plan: bin_uop_audit table with metadata
		// column; lineside_buckets table; inventory_delta_dedup
		// table. The shadow column / shadow table / per-station
		// flip flag are absent — they served the rollable cutover
		// machinery, which we don't need without a production
		// audience.
		{17, "uop bin-as-truth: audit log + delta apply infrastructure", v17UOPBinAsTruth,
			func(q schema.Querier) bool {
				return schema.TableExists(q, "bin_uop_audit") &&
					schema.TableExists(q, "lineside_buckets") &&
					schema.TableExists(q, "inventory_delta_dedup") &&
					schema.ColumnExists(q, "bin_uop_audit", "metadata")
			}},
		{18, "add skip_auto_confirm column to orders", v18OrderSkipAutoConfirm,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "orders", "skip_auto_confirm") }},
		{19, "promote retrieve_empty from payload_desc magic string to OrderType", v19PromoteRetrieveEmptyOrderType, nil},

		// v20: UOP-threshold replenishment (C-push).
		//   - lineside_buckets.payload_code lets Core sum bins +
		//     buckets per payload for SystemUOPForPayload. Populated
		//     going-forward by Edge's capture.go at emit time; no SQL
		//     backfill — Springfield is a fresh install and no plant
		//     has the pre-feature row shape.
		//   - demand_registry.replenish_uop_threshold is the per-
		//     (loader, payload) trigger value the threshold monitor
		//     compares against. Default 0 = opt-out / legacy bin-count.
		{20, "uop-threshold replenishment: payload_code + replenish_uop_threshold",
			v20UOPThresholdReplenishment,
			func(q schema.Querier) bool {
				return schema.ColumnExists(q, "lineside_buckets", "payload_code") &&
					schema.ColumnExists(q, "demand_registry", "replenish_uop_threshold")
			}},

		// v21 (Round-3 Obs 8, 2026-05-21): replace lineside_buckets.node_id
		// with core_node_name. The pre-fix BIGINT node_id mixed Edge's
		// process_nodes.id with Core's nodes.id namespace, producing
		// cross-plant attribution bugs (Springfield 6883 stuck-bucket,
		// Hopkinsville Core-only orphan). The TRUNCATE that follows
		// deploy (operator action — see Round-3 SME doc) is what
		// clears the now-orphaned rows; this migration only adjusts
		// the schema shape.
		{21, "lineside_buckets node_id → core_node_name (Round-3 Obs 8)",
			v21LinesideBucketsCoreNodeName,
			func(q schema.Querier) bool {
				return schema.ColumnExists(q, "lineside_buckets", "core_node_name") &&
					!schema.ColumnExists(q, "lineside_buckets", "node_id")
			}},
		// v22 ties dedup state to a bin's load-lifecycle. Pre-fix the
		// inventory_delta_dedup PK was (station, scope_kind, scope_key),
		// which made the dedup row outlive any single load of a bin. A
		// stale Edge seq counter (deploy reset, restore from backup,
		// cache loss) could then poison the next load's delta stream —
		// observed in the field as "bin uop_remaining stuck at the load
		// value while Edge tile counts down independently". Extending
		// the PK with epoch and bumping bins.delta_epoch at every
		// lifecycle boundary (SetForProduction, ClearForReuseTx) gives
		// each load its own dedup space.
		{22, "bins.delta_epoch + inventory_delta_dedup PK epoch column",
			v22BinDeltaEpoch,
			func(q schema.Querier) bool {
				return schema.ColumnExists(q, "bins", "delta_epoch") &&
					schema.ColumnExists(q, "inventory_delta_dedup", "epoch")
			}},

		// v23 (complex-order buried-reshuffle scope, v7) adds the
		// pending_restocks table. Closes the crash-recovery gap left
		// by the v6 in-memory restoreRegistry: when the restore-
		// blockers toggle is on, the planned restock state is
		// persisted at listener-registration time so a Core restart
		// can re-register the listener instead of dropping it on the
		// floor (and leaving blockers in shuffle slots forever).
		//
		// One row per registered listener; deleted on listener fire,
		// parent cancel, parent fail, and stale-row sweep at boot.
		{23, "add pending_restocks table for crash-safe restore listeners",
			v23PendingRestocks,
			func(q schema.Querier) bool { return schema.TableExists(q, "pending_restocks") }},

		// v24 (post-v7 cleanup) adds the pending_lane_extensions table.
		// Same shape as pending_restocks but for the lane-lock
		// extension listener in expose mode. Persisting the target bin
		// ID at scheduling time replaces the v7-era at-fire-time
		// derivation (walk lane, exclude blockers) — which only worked
		// because of a contextual invariant (lane locked, no
		// unrelated bins) that a future lane-lock refactor could
		// silently break.
		{24, "add pending_lane_extensions table for crash-safe lane-hold listeners",
			v24PendingLaneExtensions,
			func(q schema.Querier) bool { return schema.TableExists(q, "pending_lane_extensions") }},

		// v25 adds nodes.claimed_by — the store dual of bins.claimed_by — so a
		// destination slot can be atomically claimed at dispatch (Hopkinsville
		// #115/#117: two complex orders dispatching into the same supermarket
		// slot). MUST be a real versioned migration, not folded into the v6
		// legacy block: every node read selects n.claimed_by, so a DB missing the
		// column fails ALL node queries (no nodes on Core/Edge). The verify gate
		// makes the self-heal re-run it on any DB where the column is absent.
		{25, "add nodes.claimed_by slot claim (store dual of bins.claimed_by)",
			v25SlotClaiming,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "nodes", "claimed_by") }},

		// v26 adds orders.sibling_order_uuid — the Core mirror of Edge's
		// orders.sibling_order_id. Edge sends TypeOrderSiblingLink after a
		// two-robot swap pair is created so Core can model the pair (the two
		// legs arrive as independent ComplexOrderRequests). Stored as the
		// edge UUID, not a resolved id FK, so arrival order doesn't matter.
		{26, "add sibling_order_uuid column to orders",
			v26OrderSiblingUUID,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "orders", "sibling_order_uuid") }},

		// v27 adds the dashboards table — saved floor-display definitions
		// for the dashboard platform (task board + future kinds). A
		// dashboard is a named, station-scoped view of Core's live data,
		// served chromeless at /dashboard/{id}. Pure presentation config;
		// it owns no operational state, so there is no data to backfill.
		{27, "add dashboards table for the floor display platform",
			v27Dashboards,
			func(q schema.Querier) bool { return schema.TableExists(q, "dashboards") }},

		// v28 promotes bin_uop_audit to the first-class inventory event log
		// (inventory refactor §16 PR 2): node_id / station / detail JSONB +
		// a (op, applied_at) index for op-filtered timelines (the footprint
		// velocity query, §16 PR 1). Additive — existing rows get NULLs.
		{28, "enrich bin_uop_audit with node_id/station/detail + (op, applied_at) index",
			v28BinUOPAuditEnrich,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "bin_uop_audit", "node_id") }},

		// v29 adds the per-mission robot-alarm snapshot column for the
		// failure-Pareto enrichment (Q-026). Additive; populated when a mission
		// ends FAILED (write side is the remaining Q-026 ingestion).
		{29, "add mission_telemetry.robot_alarms_json for the failure-Pareto robot-alarm snapshot (Q-026)",
			v29MissionRobotAlarms,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "mission_telemetry", "robot_alarms_json") }},

		// v30 adds cell_config — the operator-defined grouping of production
		// Processes into named cells for the /missions Cells D section and the
		// /heartbeat kiosk (Phase E, Q-025). A cell groups one primary Process
		// plus optional sub-Processes; process ids match cell_part_events.process_id
		// (the Process grain the PLC counters tick at — NOT process nodes, which
		// are the bin path). No seed data: plant cells are configured via
		// /admin/cells after deploy.
		{30, "add cell_config for operator-defined production-cell grouping (Q-025, Phase E)",
			v30CellConfig,
			func(q schema.Querier) bool { return schema.TableExists(q, "cell_config") }},
		{31, "add payloads.robot_group for SEER robot-dispatch group selection",
			v31PayloadRobotGroup,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "payloads", "robot_group") }},

		// v32 adds downtime_events + downtime_event_dedup for persisted downtime
		// start/end events (G9). Partitioned monthly by started_at (same pattern
		// as cell_part_events). Replaces derived-only gap analysis with explicit
		// persisted events for OEE availability dashboards. (Authored as v31 on the
		// sim branch; renumbered to v32 when local-dev-env rebased onto main, which
		// carries payloads.robot_group as v31.)
		{32, "add downtime_events for persisted downtime start/end events (G9)",
			v32DowntimeEvents,
			func(q schema.Querier) bool { return schema.TableExists(q, "downtime_events") }},
		{33, "add edge_cells for the auto-derived cell catalog (Q-034)",
			v33EdgeCells,
			func(q schema.Querier) bool { return schema.TableExists(q, "edge_cells") }},
		// v34: Core-owned bin-loader aggregate. The loader's identity +
		// per-position/per-payload replenishment config move from Edge
		// style_node_claims to Core. These tables back the Core-owned loader read
		// path — the aggregate the Edge syncs from and resolves loaders against.
		// UNIQUE(position_node_id) is THE invariant — one payload per home
		// position, one loader per node — making the SLN_002 misconfiguration
		// unrepresentable. min_stock/uop_threshold default 0 (no silent floor; the
		// magic-2 default was removed). buffer_dest models the overflow area (SME
		// Q4 — runtime resolution lands with the read-cutover). No UNIQUE on
		// (loader_id, payload_code) for homes: same payload on two home positions
		// is legitimate (D1, allow+warn).
		{34, "add bin-loader aggregate (bin_loaders/homes/payloads) for the Core-owned loader cutover",
			v34BinLoaderAggregate,
			func(q schema.Querier) bool {
				return schema.TableExists(q, "bin_loaders") &&
					schema.TableExists(q, "bin_loader_homes") &&
					schema.TableExists(q, "bin_loader_payloads")
			}},
		// v35: per-position ordering for dedicated_positions loaders. The
		// Nodes-page grid-drag editor lets an operator drag position nodes into a
		// loader and reorder them; sort_order persists that sequence (the physical
		// position order) so it survives reload and can drive future layout UI.
		// Additive, default 0 — existing homes keep their implicit
		// position_node_id order until first reordered.
		{35, "add sort_order to bin_loader_homes for drag-reorder of dedicated positions",
			v35LoaderHomeSortOrder,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "bin_loader_homes", "sort_order") }},
		// v36: archived_at for loader SOFT-delete (step 7). DeleteLoader will set this
		// instead of cascading the loader + its homes/payloads away, so the stamped
		// bin_uop_audit history survives a retired loader. Additive, NULL = active.
		{36, "add bin_loaders.archived_at for loader soft-delete",
			v36LoaderArchivedAt,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "bin_loaders", "archived_at") }},
		// v37: loader_id on bin_uop_audit — the resolved loader surrogate stamped at
		// EVENT time so loads (set_for_production) and unloads (release-family ops) group
		// per loader. PLAIN value column, NO REFERENCES / NO cascade: archiving or
		// deleting a loader must NOT destroy its audit history, and a node later
		// reassigned to a different loader keeps each event's historical attribution.
		{37, "add bin_uop_audit.loader_id (non-cascading) for per-loader load/unload analytics",
			v37BinUOPAuditLoaderID,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "bin_uop_audit", "loader_id") }},
		// v38: loader_id on demand_registry — the loader IDENTITY behind a binding, set
		// from the aggregate at re-derive time (the step-4 cutover). The threshold
		// monitor mints LoaderKey="loader:<id>" from it onto the signal so the Edge
		// resolves the loader by its token instead of core_node_name (which doubles as
		// identity+member today). NULL for legacy ClaimSync-populated rows. Plain value
		// (no FK — the registry is rebuilt full-state per station, not cascaded).
		{38, "add demand_registry.loader_id for the loader-identity cutover",
			v38DemandRegistryLoaderID,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "demand_registry", "loader_id") }},
		// v39: drop bin_loaders.core_node_name (+ its UNIQUE(core_node_name, role)).
		// The loader's identity is its surrogate id (minted onto the wire as the
		// loader_key token); every delivery target is an explicit member node
		// (windows/positions, FK'd to nodes). So the loader no longer borrows the
		// universal node id, and the synthetic anchor string a multi-window loader had
		// to invent is gone. Postgres drops the dependent UNIQUE with the column. The
		// aggregate is rebuilt by seeddev / migrateloaders, so there is no data to keep.
		{39, "drop bin_loaders.core_node_name + its UNIQUE (loader identity is the surrogate id)",
			v39DropLoaderCoreNodeName,
			func(q schema.Querier) bool { return !schema.ColumnExists(q, "bin_loaders", "core_node_name") }},
	}

	// Record the head version for LatestMigrationVersion, derived from the list
	// itself — adding a migration above updates it with no separate bookkeeping.
	if n := len(migrations); n > 0 {
		latestMigrationVersion = migrations[n-1].version
	}

	for _, m := range migrations {
		var applied bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, m.version).Scan(&applied)

		// Self-heal check: if recorded as applied but the post-
		// condition is missing, treat as not-applied so the
		// transactional re-run below restores it.
		if applied && m.verify != nil && !m.verify(db.DB) {
			log.Printf("migrations: v%d (%s) recorded as applied but post-condition fails — re-running",
				m.version, m.name)
			if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = $1`, m.version); err != nil {
				return fmt.Errorf("clear stale schema_migrations row v%d: %w", m.version, err)
			}
			applied = false
		}
		if applied {
			continue
		}
		if err := db.runOneMigration(m.version, m.name, m.fn); err != nil {
			return err
		}
	}
	return nil
}

// verifyV15BinTransitState checks that BOTH the synthetic _TRANSIT
// node row AND the bins.anomaly_at column are present. v15 is the only
// migration that touches more than one piece of schema, so the verify
// is a small composite rather than a one-liner.
func verifyV15BinTransitState(q schema.Querier) bool {
	if !schema.ColumnExists(q, "bins", "anomaly_at") {
		return false
	}
	var exists bool
	q.QueryRow(`SELECT EXISTS (SELECT 1 FROM nodes WHERE name='_TRANSIT' AND is_synthetic=true)`).Scan(&exists)
	return exists
}

// runOneMigration wraps a single migration's DDL/DML and its
// schema_migrations row insert in one transaction. On any error, the
// transaction rolls back and the migration is re-attempted on the next
// startup. Migrations are written to be idempotent so re-runs are
// always safe.
func (db *DB) runOneMigration(version int, name string, fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migration v%d (%s): begin tx: %w", version, name, err)
	}
	defer tx.Rollback() // no-op after Commit

	if err := fn(tx); err != nil {
		return fmt.Errorf("migration v%d (%s): %w", version, name, err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
		return fmt.Errorf("migration v%d (%s): record version: %w", version, name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration v%d (%s): commit: %w", version, name, err)
	}
	return nil
}

// v1BooleanColumns converts INTEGER boolean columns to native BOOLEAN.
func v1BooleanColumns(tx *sql.Tx) error {
	conversions := []struct{ table, column, defVal string }{
		{"nodes", "is_synthetic", "false"},
		{"nodes", "enabled", "true"},
		{"node_types", "is_synthetic", "false"},
		{"bins", "manifest_confirmed", "false"},
		{"bins", "locked", "false"},
	}
	for _, c := range conversions {
		if !schema.TableExists(tx, c.table) || !schema.ColumnExists(tx, c.table, c.column) {
			continue
		}
		if schema.ColumnType(tx, c.table, c.column) == "boolean" {
			continue
		}
		if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT`, c.table, c.column)); err != nil {
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s TYPE BOOLEAN USING %s != 0`, c.table, c.column, c.column)); err != nil {
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s`, c.table, c.column, c.defVal)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_bins_locked`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_bins_locked ON bins(locked) WHERE locked = true`); err != nil {
		return err
	}
	return nil
}

// v2DepthColumn adds a depth column to nodes and migrates data from node_properties.
func v2DepthColumn(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS depth INTEGER`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE nodes SET depth = CAST(np.value AS INTEGER)
		FROM node_properties np
		WHERE np.node_id = nodes.id AND np.key = 'depth' AND nodes.depth IS NULL`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM node_properties WHERE key = 'depth'`); err != nil {
		return err
	}
	return nil
}

// v3DropDeadColumns removes columns that are no longer used.
func v3DropDeadColumns(tx *sql.Tx) error {
	drops := []struct{ table, column string }{
		{"orders", "source_node_id"},
		{"orders", "dest_node_id"},
		{"orders", "factory_id"},
		{"edge_registry", "factory_id"},
	}
	for _, d := range drops {
		if !schema.TableExists(tx, d.table) {
			continue
		}
		if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN IF EXISTS %s`, d.table, d.column)); err != nil {
			return err
		}
	}
	return nil
}

// v4DropOrderPayloadID removes the vestigial payload_id column from orders.
func v4DropOrderPayloadID(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE orders DROP COLUMN IF EXISTS payload_id`)
	return err
}

// v18OrderSkipAutoConfirm adds skip_auto_confirm to orders so side-cycle
// L1/U1 orders can opt out of Core's reconciliation auto-confirm sweep.
func v18OrderSkipAutoConfirm(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS skip_auto_confirm BOOLEAN NOT NULL DEFAULT false`)
	return err
}

// v19PromoteRetrieveEmptyOrderType moves the retrieve_empty signal off the
// payload_desc free-text field and onto a first-class OrderType value. Before
// this migration, retrieve_empty orders had order_type='retrieve' and
// payload_desc='retrieve_empty' (a magic string sniffed by planner + scanner).
// After: order_type='retrieve_empty' and payload_desc cleared on those rows
// so the column reverts to its single purpose (operator-supplied note).
//
// Historical rows in mission_telemetry and other denormalized order_type
// columns are intentionally NOT backfilled — telemetry is read-only stats
// and the inconsistency doesn't affect behavior. New rows written post-
// migration will carry the correct OrderType going forward. See
// SHINGO_TODO.md "Refactor: collapse single-bin OrderTypes" for the larger
// follow-up that would obviate this column entirely.
func v19PromoteRetrieveEmptyOrderType(tx *sql.Tx) error {
	_, err := tx.Exec(`UPDATE orders SET order_type = 'retrieve_empty', payload_desc = '' WHERE payload_desc = 'retrieve_empty' AND order_type = 'retrieve'`)
	return err
}

// v20UOPThresholdReplenishment adds the two columns the UOP-threshold
// C-push architecture needs at Core:
//
//   - lineside_buckets.payload_code lets SystemUOPForPayload sum bins
//     and buckets for the same payload. Edge populates this at capture
//     time from the order context. No SQL backfill — Springfield is a
//     fresh install and no plant has the pre-feature row shape that
//     would need one. If/when a future plant needs backfill, design
//     then with bin_uop_audit correlation (each capture event records
//     the bin's order_id and payload_code, so joining gives correct
//     attribution).
//
//   - demand_registry.replenish_uop_threshold is the per-(loader,
//     payload) trigger value. SyncRegistry persists it; the threshold
//     monitor compares combined in-loop UOP against it on every bin
//     update / bucket delta apply. Default 0 = opt-out (Core never
//     monitors that pair, legacy bin-count at Edge preserved).
func v20UOPThresholdReplenishment(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE lineside_buckets ADD COLUMN IF NOT EXISTS payload_code TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add lineside_buckets.payload_code: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_lineside_buckets_payload ON lineside_buckets(payload_code)`); err != nil {
		return fmt.Errorf("create idx_lineside_buckets_payload: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE demand_registry ADD COLUMN IF NOT EXISTS replenish_uop_threshold INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("add demand_registry.replenish_uop_threshold: %w", err)
	}
	return nil
}

// v21LinesideBucketsCoreNodeName rewrites lineside_buckets so the
// node_id BIGINT column is replaced with core_node_name TEXT. Round-3
// Obs 8 — the int64 namespace mix between Edge and Core was the
// source of cross-plant attribution drift.
//
// The migration drops the old column, the unique constraint on
// (station, node_id, ...), and the (node_id, style_id) index, then
// adds the new column / constraint / index. Existing rows lose their
// node attribution — that's intentional and aligned with the SME doc's
// post-deploy TRUNCATE step: bad data from the pre-fix days is not
// recoverable (we don't know which CoreNodeName the int64 was supposed
// to mean), and the next capture/drain cycle from Edge re-populates
// the table cleanly using the new wire shape. Operators run the
// TRUNCATE explicitly after deploy; the migration is a pure schema
// change.
//
// CASCADE is required on the UNIQUE drop because the auto-generated
// constraint name embeds the column list; we drop it by introspecting
// pg_constraint and using ALTER TABLE ... DROP CONSTRAINT.
func v21LinesideBucketsCoreNodeName(tx *sql.Tx) error {
	// 1. Drop the old UNIQUE constraint (it references node_id).
	if _, err := tx.Exec(`
		DO $$
		DECLARE c RECORD;
		BEGIN
			FOR c IN
				SELECT con.conname
				FROM pg_constraint con
				JOIN pg_class rel ON rel.oid = con.conrelid
				WHERE rel.relname = 'lineside_buckets'
				  AND con.contype = 'u'
			LOOP
				EXECUTE 'ALTER TABLE lineside_buckets DROP CONSTRAINT ' || quote_ident(c.conname);
			END LOOP;
		END $$`); err != nil {
		return fmt.Errorf("drop lineside_buckets unique constraint: %w", err)
	}

	// 2. Drop the old idx_lineside_buckets_node_style (was on node_id, style_id).
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_lineside_buckets_node_style`); err != nil {
		return fmt.Errorf("drop idx_lineside_buckets_node_style: %w", err)
	}

	// 3. Drop the old node_id column.
	if _, err := tx.Exec(`ALTER TABLE lineside_buckets DROP COLUMN IF EXISTS node_id`); err != nil {
		return fmt.Errorf("drop lineside_buckets.node_id: %w", err)
	}

	// 4. Add core_node_name. NOT NULL with a DEFAULT '' so existing
	//    orphaned rows (about to be TRUNCATEd by the operator post-deploy)
	//    don't break the column add. IF NOT EXISTS because schema.Apply
	//    on a fresh DB created the new shape ahead of the migration
	//    pipeline.
	if _, err := tx.Exec(`ALTER TABLE lineside_buckets ADD COLUMN IF NOT EXISTS core_node_name TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add lineside_buckets.core_node_name: %w", err)
	}

	// 5. Recreate the unique constraint and the supporting index on
	//    the new column. The constraint add is wrapped in a guard so a
	//    fresh DB (where schema.Apply already installed the new shape's
	//    UNIQUE) skips this branch — postgres has no "ADD CONSTRAINT
	//    IF NOT EXISTS" syntax.
	if _, err := tx.Exec(`DO $$
		BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint con
				JOIN pg_class rel ON rel.oid = con.conrelid
				WHERE rel.relname = 'lineside_buckets'
				  AND con.contype = 'u'
			) THEN
				ALTER TABLE lineside_buckets ADD CONSTRAINT lineside_buckets_station_core_node_pair_style_part_key UNIQUE (station, core_node_name, pair_key, style_id, part_number);
			END IF;
		END $$`); err != nil {
		return fmt.Errorf("add lineside_buckets unique constraint: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_lineside_buckets_node_style ON lineside_buckets(core_node_name, style_id)`); err != nil {
		return fmt.Errorf("create idx_lineside_buckets_node_style: %w", err)
	}

	// 6. Clear the bucket-scope inventory_delta_dedup rows. The pre-fix
	//    scope_key was "<NodeID>|<PairKey>|<StyleID>|<PartNumber>";
	//    post-fix it is "<CoreNodeName>|<PairKey>|<StyleID>|<PartNumber>".
	//    Leaving the old keys behind risks an out-of-order replay
	//    shadowing a new key whose last_seq is genuinely lower.
	//    Idempotent: a fresh DB has no rows to delete.
	if _, err := tx.Exec(`DELETE FROM inventory_delta_dedup WHERE scope_kind = 'bucket'`); err != nil {
		return fmt.Errorf("clear bucket dedup rows: %w", err)
	}
	return nil
}

// v22BinDeltaEpoch extends the bin-side delta plumbing with an epoch
// number tied to the bin's load-lifecycle. Before this, the
// inventory_delta_dedup PK was (station, scope_kind, scope_key); the
// dedup row outlived any single load of a bin so a stale Edge seq
// counter (deploy reset, restore from backup, cache loss) could poison
// the next load's delta stream — Core silently dropped Edge's deltas
// as "already-applied replays" against the prior load's last_seq.
//
// Two related changes:
//
//  1. bins.delta_epoch column, default 1. SetForProduction and
//     ClearForReuseTx bump it on every Core-controlled lifecycle
//     transition. The bin's identity persists across loads; epoch
//     labels each load distinctly.
//
//  2. inventory_delta_dedup PK gains an epoch column. Pre-existing
//     rows are backfilled to epoch=0 so they don't shadow the
//     post-migration first deltas (which arrive at epoch >= 1).
//
// Buckets are untouched here — bucket scope deltas carry epoch=0 on
// the wire, and Core handles bucket dedup cleanup via the existing
// qty→0 GC + admin-delete paths (see store/inventory/inventory.go
// DeleteLinesideBucket). Bucket lifecycle is Edge-observed (qty
// zeroing), not Core-controlled, which doesn't map cleanly to the
// epoch-on-load model. If buckets ever exhibit the same drift class,
// a separate migration can add bucket-side epoch with bucket-life
// boundaries chosen at that time.
func v22BinDeltaEpoch(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE bins ADD COLUMN IF NOT EXISTS delta_epoch BIGINT NOT NULL DEFAULT 1`); err != nil {
		return fmt.Errorf("add bins.delta_epoch: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE inventory_delta_dedup ADD COLUMN IF NOT EXISTS epoch BIGINT NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("add inventory_delta_dedup.epoch: %w", err)
	}
	// PK swap: drop the (station, scope_kind, scope_key) PK and add
	// the (station, scope_kind, scope_key, epoch) PK. Existing rows
	// keep their epoch=0 default. Postgres has no atomic "swap PK"
	// in one statement — drop + add. The pg_constraint check guards
	// idempotency on re-run.
	if _, err := tx.Exec(`DO $$
		BEGIN
			IF EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conname = 'inventory_delta_dedup_pkey'
			) THEN
				ALTER TABLE inventory_delta_dedup DROP CONSTRAINT inventory_delta_dedup_pkey;
			END IF;
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint con
				JOIN pg_class rel ON rel.oid = con.conrelid
				WHERE rel.relname = 'inventory_delta_dedup'
				  AND con.contype = 'p'
			) THEN
				ALTER TABLE inventory_delta_dedup ADD PRIMARY KEY (station, scope_kind, scope_key, epoch);
			END IF;
		END $$`); err != nil {
		return fmt.Errorf("swap inventory_delta_dedup PK to include epoch: %w", err)
	}
	return nil
}

// v7DropDefaultManifestJSON removes the vestigial default_manifest_json column.
func v7DropDefaultManifestJSON(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE payloads DROP COLUMN IF EXISTS default_manifest_json`)
	return err
}

// v6LegacyConsolidation runs all legacy (previously unversioned) migrations once.
// Each sub-migration is idempotent to handle databases of any age.
func v6LegacyConsolidation(tx *sql.Tx) error {
	if err := migrateNodeTypes(tx); err != nil {
		return fmt.Errorf("node types: %w", err)
	}
	if err := migrateShallowLanes(tx); err != nil {
		return fmt.Errorf("shallow lanes: %w", err)
	}
	if err := migrateVendorLocation(tx); err != nil {
		return fmt.Errorf("vendor location: %w", err)
	}
	if err := migrateIsSynthetic(tx); err != nil {
		return fmt.Errorf("is_synthetic: %w", err)
	}
	if err := migrateDropCapacity(tx); err != nil {
		return fmt.Errorf("drop capacity: %w", err)
	}
	if err := migrateDropNodeType(tx); err != nil {
		return fmt.Errorf("drop node_type: %w", err)
	}
	if err := migrateCMSTransactions(tx); err != nil {
		return fmt.Errorf("cms transactions: %w", err)
	}
	if err := migrateStepsJSON(tx); err != nil {
		return fmt.Errorf("steps_json: %w", err)
	}
	if err := migrateBinClaiming(tx); err != nil {
		return fmt.Errorf("bin claiming: %w", err)
	}
	if err := migrateDeliveryNodeIndex(tx); err != nil {
		return fmt.Errorf("delivery node index: %w", err)
	}
	if err := migrateBinsCommandCenter(tx); err != nil {
		return fmt.Errorf("bins command center: %w", err)
	}
	return nil
}

// --- Legacy migrations (idempotent, retained for v6 consolidation) ---

func migrateStepsJSON(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS steps_json TEXT NOT NULL DEFAULT ''`)
	return err
}

func migrateVendorLocation(tx *sql.Tx) error {
	if !schema.ColumnExists(tx, "nodes", "vendor_location") {
		return nil
	}
	if _, err := tx.Exec(`UPDATE nodes SET name = vendor_location WHERE (name = '' OR name IS NULL) AND vendor_location != ''`); err != nil {
		return err
	}
	_, err := tx.Exec(`ALTER TABLE nodes DROP COLUMN IF EXISTS vendor_location`)
	return err
}

func migrateIsSynthetic(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS is_synthetic BOOLEAN NOT NULL DEFAULT false`); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE nodes SET is_synthetic = true WHERE node_type_id IN (SELECT id FROM node_types WHERE is_synthetic = true) AND is_synthetic = false`)
	return err
}

func migrateDropCapacity(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE nodes DROP COLUMN IF EXISTS capacity`)
	return err
}

func migrateDropNodeType(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE nodes DROP COLUMN IF EXISTS node_type`)
	return err
}

func migrateCMSTransactions(tx *sql.Tx) error {
	if !schema.TableExists(tx, "cms_transactions") {
		return nil
	}
	if schema.ColumnExists(tx, "cms_transactions", "txn_type") {
		return nil
	}
	if schema.ColumnExists(tx, "cms_transactions", "direction") {
		if _, err := tx.Exec(`ALTER TABLE cms_transactions RENAME COLUMN direction TO txn_type`); err != nil {
			return err
		}
	}
	if schema.ColumnExists(tx, "cms_transactions", "quantity") {
		if _, err := tx.Exec(`ALTER TABLE cms_transactions RENAME COLUMN quantity TO delta`); err != nil {
			return err
		}
	}
	newCols := []struct{ name, def string }{
		{"qty_before", "INTEGER NOT NULL DEFAULT 0"},
		{"qty_after", "INTEGER NOT NULL DEFAULT 0"},
		{"bin_label", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, c := range newCols {
		if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE cms_transactions ADD COLUMN IF NOT EXISTS %s %s`, c.name, c.def)); err != nil {
			return err
		}
	}
	return nil
}

func migrateBinClaiming(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE bins ADD COLUMN IF NOT EXISTS claimed_by BIGINT REFERENCES orders(id)`); err != nil {
		return err
	}
	_, err := tx.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS bin_id BIGINT REFERENCES bins(id)`)
	return err
}

func migrateDeliveryNodeIndex(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_orders_delivery_node ON orders(delivery_node)`)
	return err
}

// v25SlotClaiming adds the store dual of bins.claimed_by: a per-node
// destination claim so two orders can't be dispatched into the same slot.
// Before this, slot selection relied on a non-atomic check keyed on
// orders.delivery_node, which (a) two near-simultaneous releases could both
// pass and (b) never saw a multi-leg order's intermediate drop-off (its
// delivery_node is the final leg). The node-level claim closes both: it's an
// atomic CAS on the actual destination node, wherever it sits in the route.
// Mirrors migrateBinClaiming. The partial index keeps "is this slot claimed"
// lookups cheap without indexing the unclaimed majority.
func v25SlotClaiming(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS claimed_by BIGINT REFERENCES orders(id)`); err != nil {
		return fmt.Errorf("add nodes.claimed_by: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_nodes_claimed_by ON nodes(claimed_by) WHERE claimed_by IS NOT NULL`); err != nil {
		return fmt.Errorf("create idx_nodes_claimed_by: %w", err)
	}
	return nil
}

func v26OrderSiblingUUID(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS sibling_order_uuid TEXT NOT NULL DEFAULT ''`)
	return err
}

// v27Dashboards creates the dashboards table backing the floor display
// platform. Mirrors the baseline DDL in schema/postgres_ddl.go. A dashboard
// is a saved, station-scoped view of Core's live data (task board today,
// other kinds later) rendered chromeless for wall monitors. Idempotent
// CREATE ... IF NOT EXISTS so fresh DBs (which got the table from the
// baseline) and upgraded DBs both converge.
func v27Dashboards(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS dashboards (
		id            BIGSERIAL PRIMARY KEY,
		name          TEXT NOT NULL,
		kind          TEXT NOT NULL DEFAULT 'task-board',
		stations_json TEXT NOT NULL DEFAULT '[]',
		config_json   TEXT NOT NULL DEFAULT '{}',
		enabled       BOOLEAN NOT NULL DEFAULT true,
		sort_order    INTEGER NOT NULL DEFAULT 0,
		created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	return err
}

// v28BinUOPAuditEnrich promotes bin_uop_audit to the first-class inventory
// event log (inventory refactor §16 PR 2). Adds node_id / station / detail
// JSONB and a (op, applied_at DESC) index for op-filtered timelines such as
// the footprint loaded/unloaded velocity query (§16 PR 1). Additive only —
// existing rows get NULL node_id/detail and ” station. ADD COLUMN IF NOT
// EXISTS is apply-once-idempotent.
func v28BinUOPAuditEnrich(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE bin_uop_audit ADD COLUMN IF NOT EXISTS node_id BIGINT`,
		`ALTER TABLE bin_uop_audit ADD COLUMN IF NOT EXISTS station TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bin_uop_audit ADD COLUMN IF NOT EXISTS detail JSONB`,
		`CREATE INDEX IF NOT EXISTS idx_bin_uop_audit_op_time ON bin_uop_audit(op, applied_at DESC)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v28 bin_uop_audit enrich: %w", err)
		}
	}
	return nil
}

// v29MissionRobotAlarms adds the per-mission robot-alarm snapshot column
// (Q-026). When a mission ends FAILED, the active robot_alarm_log codes in its
// window are snapshotted here as a JSON array of {code,severity,desc,…} so the
// failure Pareto can classify the real hardware fault. Additive; existing rows
// get NULL.
func v29MissionRobotAlarms(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE mission_telemetry ADD COLUMN IF NOT EXISTS robot_alarms_json JSONB`); err != nil {
		return fmt.Errorf("v29 mission_telemetry.robot_alarms_json: %w", err)
	}
	return nil
}

// v30CellConfig creates cell_config — the operator-defined grouping of
// production Processes into named cells (Phase E, Q-025). cell_id is the
// operator-chosen key (e.g. "SNF2"); station ties the cell to its
// cell_part_events stream (cell_part_events.cell_id = station). primary and
// sub process ids match cell_part_events.process_id (the Process grain). The
// sub list is JSONB rather than BIGINT[] because the pgx/database-sql path has
// no array scanner and the codebase's array idiom is JSONB (cf. *_json
// columns). No seed data — cells are configured per plant via /admin/cells.
func v30CellConfig(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS cell_config (
			cell_id            TEXT PRIMARY KEY,
			station            TEXT NOT NULL,
			primary_process_id BIGINT NOT NULL,
			sub_process_ids    JSONB NOT NULL DEFAULT '[]'::jsonb,
			display_name       TEXT NOT NULL DEFAULT '',
			updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS cell_config_station_idx ON cell_config (station)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v30 cell_config: %w", err)
		}
	}
	return nil
}

// v31PayloadRobotGroup adds payloads.robot_group — the SEER robot-dispatch group
// the dispatcher stamps onto SetOrderRequest.Group for moves of this payload.
// Default ” = unset = SEER's own default robot assignment (backward-compatible).
func v31PayloadRobotGroup(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE payloads ADD COLUMN IF NOT EXISTS robot_group TEXT NOT NULL DEFAULT ''`)
	return err
}

// v32DowntimeEvents creates downtime_events (partitioned monthly by started_at)
// and downtime_event_dedup for the G9 persisted-downtime pipeline. Same shape
// as cell_part_events: append-only event log + small dedup guard. Index on
// (station, started_at) mirrors the heartbeat query pattern.
func v32DowntimeEvents(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS downtime_events (
			id              BIGSERIAL,
			station         TEXT    NOT NULL,
			plc_name        TEXT    NOT NULL,
			reason          TEXT    NOT NULL DEFAULT '',
			started_at      TIMESTAMPTZ NOT NULL,
			ended_at        TIMESTAMPTZ,
			duration_ms     BIGINT  NOT NULL DEFAULT 0,
			edge_event_id   BIGINT  NOT NULL DEFAULT 0
		) PARTITION BY RANGE (started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_downtime_events_station_time ON downtime_events (station, started_at)`,
		`CREATE TABLE IF NOT EXISTS downtime_event_dedup (
			station         TEXT    NOT NULL,
			edge_event_id   BIGINT NOT NULL,
			applied_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (station, edge_event_id)
		)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v32 downtime_events: %w", err)
		}
	}
	return nil
}

// v33EdgeCells creates the auto-derived cell catalog (Q-034). One row per
// (station, cell_label) — a cell is a PLC the edge reported. bindings is the
// JSONB array of its process tuples. last_seen + stale track reconciliation:
// upserts refresh last_seen and clear stale; cells absent from a newer catalog
// are marked stale rather than deleted (the scenesync ghost lesson — keep
// history visible).
func v33EdgeCells(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS edge_cells (
			station     TEXT        NOT NULL,
			cell_label  TEXT        NOT NULL,
			bindings    JSONB       NOT NULL DEFAULT '[]'::jsonb,
			first_seen  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_seen   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			stale       BOOLEAN     NOT NULL DEFAULT FALSE,
			PRIMARY KEY (station, cell_label)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_edge_cells_station ON edge_cells (station)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v33 edge_cells: %w", err)
		}
	}
	return nil
}

// v34BinLoaderAggregate creates the Core-owned bin-loader aggregate (loader
// refactor cutover). See the migration-list comment for the design. All three
// tables are additive and have no runtime reader until the LoaderStore cutover,
// so applying this on a live plant changes no behavior.
func v34BinLoaderAggregate(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS bin_loaders (
			id              BIGSERIAL PRIMARY KEY,
			name            TEXT        NOT NULL,
			core_node_name  TEXT        NOT NULL,
			role            TEXT        NOT NULL CHECK (role IN ('produce','consume')),
			layout          TEXT        NOT NULL CHECK (layout IN ('shared_window','dedicated_positions')),
			replenishment   TEXT        NOT NULL CHECK (replenishment IN ('auto','operator')),
			outbound_dest   TEXT        NOT NULL DEFAULT '',
			inbound_source  TEXT        NOT NULL DEFAULT '',
			buffer_dest     TEXT        NOT NULL DEFAULT '',
			config_gen      BIGINT      NOT NULL DEFAULT 1,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (core_node_name, role)
		)`,
		// dedicated_positions layout: one payload per physical position. The
		// global UNIQUE(position_node_id) is the load-bearing invariant.
		`CREATE TABLE IF NOT EXISTS bin_loader_homes (
			loader_id        BIGINT  NOT NULL REFERENCES bin_loaders(id) ON DELETE CASCADE,
			position_node_id BIGINT  NOT NULL REFERENCES nodes(id),
			payload_code     TEXT    NOT NULL,
			min_stock        INTEGER NOT NULL DEFAULT 0,
			uop_threshold    INTEGER NOT NULL DEFAULT 0,
			UNIQUE (position_node_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_bin_loader_homes_loader ON bin_loader_homes (loader_id)`,
		// shared_window layout: the allowed payload set on a single window.
		`CREATE TABLE IF NOT EXISTS bin_loader_payloads (
			loader_id     BIGINT  NOT NULL REFERENCES bin_loaders(id) ON DELETE CASCADE,
			payload_code  TEXT    NOT NULL,
			min_stock     INTEGER NOT NULL DEFAULT 0,
			uop_threshold INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (loader_id, payload_code)
		)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v34 bin-loader aggregate: %w", err)
		}
	}
	return nil
}

// v35LoaderHomeSortOrder adds the per-position ordering column used by the
// Nodes-page grid-drag loader editor. Additive + idempotent.
func v35LoaderHomeSortOrder(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE bin_loader_homes ADD COLUMN IF NOT EXISTS sort_order INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("v35 loader home sort_order: %w", err)
	}
	return nil
}

// v36LoaderArchivedAt adds the soft-delete marker for loaders (step 7). NULL = active;
// DeleteLoader sets NOW() instead of cascading away the loader + its analytics history.
// Additive + idempotent.
func v36LoaderArchivedAt(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE bin_loaders ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ`); err != nil {
		return fmt.Errorf("v36 loader archived_at: %w", err)
	}
	return nil
}

// v37BinUOPAuditLoaderID adds the resolved loader surrogate to the inventory event log.
// PLAIN BIGINT (NO REFERENCES / NO cascade) stamped at event time so the historical
// attribution survives a node reassignment or a loader archive/delete. Partial index
// on (loader_id, applied_at) for the per-loader analytics rollup. Additive + idempotent.
func v37BinUOPAuditLoaderID(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE bin_uop_audit ADD COLUMN IF NOT EXISTS loader_id BIGINT`,
		`CREATE INDEX IF NOT EXISTS idx_bin_uop_audit_loader ON bin_uop_audit (loader_id, applied_at) WHERE loader_id IS NOT NULL`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v37 bin_uop_audit loader_id: %w", err)
		}
	}
	return nil
}

// v38DemandRegistryLoaderID adds the loader-identity column to demand_registry (the
// step-4 cutover). NULL for legacy ClaimSync rows; set from the aggregate when the
// registry is re-derived. The threshold monitor mints the loader_key from it. Additive
// + idempotent.
func v38DemandRegistryLoaderID(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE demand_registry ADD COLUMN IF NOT EXISTS loader_id BIGINT`); err != nil {
		return fmt.Errorf("v38 demand_registry loader_id: %w", err)
	}
	return nil
}

// v39DropLoaderCoreNodeName drops the loader's borrowed universal node id and its
// identity UNIQUE. After the identity cutover a loader is keyed by its surrogate id
// (the loader_key token on the wire) and resolves every delivery target through
// explicit member nodes, so core_node_name — synthetic for a multi-window loader —
// has no remaining job. DROP COLUMN cascades the UNIQUE(core_node_name, role). The
// per-position node ids in bin_loader_homes (REFERENCES nodes(id)) are untouched.
func v39DropLoaderCoreNodeName(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE bin_loaders DROP CONSTRAINT IF EXISTS bin_loaders_core_node_name_role_key`,
		`ALTER TABLE bin_loaders DROP COLUMN IF EXISTS core_node_name`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v39 drop bin_loaders.core_node_name: %w", err)
		}
	}
	return nil
}

func migrateBinsCommandCenter(tx *sql.Tx) error {
	cols := []struct{ name, def string }{
		{"locked", "BOOLEAN NOT NULL DEFAULT false"},
		{"locked_by", "TEXT NOT NULL DEFAULT ''"},
		{"locked_at", "TIMESTAMPTZ"},
		{"last_counted_at", "TIMESTAMPTZ"},
		{"last_counted_by", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, c := range cols {
		if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE bins ADD COLUMN IF NOT EXISTS %s %s`, c.name, c.def)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_bins_locked ON bins(locked) WHERE locked = true`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_bins_label_unique ON bins(label) WHERE label != ''`)
	return err
}

func migrateNodeTypes(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS node_type_id BIGINT`); err != nil {
		return fmt.Errorf("add node_type_id: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE nodes ADD COLUMN IF NOT EXISTS parent_id BIGINT REFERENCES nodes(id)`); err != nil {
		return fmt.Errorf("add parent_id: %w", err)
	}

	for _, rename := range [][2]string{
		{"SUP", "SMKT"}, {"LAN", "LANE"}, {"SHF", "SHUF"},
		{"CHG", "CHRG"}, {"OFL", "OVFL"}, {"STN", "STAG"},
		{"SMKT", "NGRP"},
	} {
		if _, err := tx.Exec(`UPDATE node_types SET code=$1 WHERE code=$2`, rename[1], rename[0]); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`UPDATE nodes SET node_type_id = NULL WHERE node_type_id IN (SELECT id FROM node_types WHERE code = 'STG')`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM node_types WHERE code = 'STG'`); err != nil {
		return err
	}

	seeds := []struct{ code, name, desc string }{
		{"LANE", "Lane", "Lane (groups depth-ordered slots)"},
		{"NGRP", "Node Group", "Node group (synthetic parent for lanes and direct nodes)"},
	}
	for _, s := range seeds {
		if _, err := tx.Exec(`INSERT INTO node_types (code, name, description, is_synthetic) VALUES ($1, $2, $3, true) ON CONFLICT (code) DO NOTHING`,
			s.code, s.name, s.desc); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`UPDATE nodes SET node_type_id = NULL WHERE node_type_id IN (SELECT id FROM node_types WHERE is_synthetic = false)`); err != nil {
		return err
	}

	var laneTypeID int64
	tx.QueryRow(`SELECT id FROM node_types WHERE code='LANE'`).Scan(&laneTypeID)
	if laneTypeID > 0 {
		if _, err := tx.Exec(`UPDATE nodes SET node_type_id = $1 WHERE node_type_id IN (SELECT id FROM node_types WHERE code = 'SHUF')`, laneTypeID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM node_types WHERE code = 'SHUF'`); err != nil {
		return err
	}

	return nil
}

// migrateShallowLanes inlines the legacy shallow-lane consolidation as
// raw SQL so it runs inside the v6 transaction. Pre-2026-05 it called
// methods on *DB which used the connection pool, escaping the
// migration's transactional scope.
func migrateShallowLanes(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT np.node_id FROM node_properties np JOIN nodes n ON n.id = np.node_id WHERE np.key = 'shallow' AND np.value = 'true'`)
	if err != nil {
		// node_properties may not exist on fresh DBs — treat as no-op.
		return nil
	}
	var shallowLaneIDs []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			shallowLaneIDs = append(shallowLaneIDs, id)
		}
	}
	rows.Close()

	for _, laneID := range shallowLaneIDs {
		var parentID sql.NullInt64
		if err := tx.QueryRow(`SELECT parent_id FROM nodes WHERE id=$1`, laneID).Scan(&parentID); err != nil {
			continue
		}
		if !parentID.Valid {
			continue
		}
		// Promote non-synthetic children to be direct children of the group.
		if _, err := tx.Exec(`UPDATE nodes SET parent_id=$1, updated_at=NOW()
			WHERE parent_id=$2 AND COALESCE(is_synthetic, false) = false`,
			parentID.Int64, laneID); err != nil {
			return err
		}
		// Clear the role property on those promoted children.
		if _, err := tx.Exec(`DELETE FROM node_properties
			WHERE key='role' AND node_id IN (
				SELECT id FROM nodes WHERE parent_id=$1
			)`, parentID.Int64); err != nil {
			return err
		}
		// Clean up the lane itself.
		if _, err := tx.Exec(`DELETE FROM node_properties WHERE node_id=$1`, laneID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM node_stations WHERE node_id=$1`, laneID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM node_payloads WHERE node_id=$1`, laneID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM nodes WHERE id=$1`, laneID); err != nil {
			return err
		}
	}
	return nil
}

// v8OrderPayloadCode adds payload_code column to orders for queued order fulfillment.
func v8OrderPayloadCode(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS payload_code TEXT NOT NULL DEFAULT ''`)
	return err
}

// v9OrderBins creates the order_bins junction table for multi-bin complex order tracking.
func v9OrderBins(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS order_bins (
		id          BIGSERIAL PRIMARY KEY,
		order_id    BIGINT NOT NULL REFERENCES orders(id),
		bin_id      BIGINT NOT NULL REFERENCES bins(id),
		step_index  INT NOT NULL,
		action      TEXT NOT NULL,
		node_name   TEXT NOT NULL,
		dest_node   TEXT NOT NULL DEFAULT '',
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("create order_bins table: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_order_bins_order ON order_bins(order_id)`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_order_bins_bin ON order_bins(bin_id)`)
	return err
}

// v5MissionTelemetryBackfill creates summary rows for historical completed orders.
func v5MissionTelemetryBackfill(tx *sql.Tx) error {
	_, err := tx.Exec(`INSERT INTO mission_telemetry
		(order_id, vendor_order_id, robot_id, station_id, order_type,
		 source_node, delivery_node, terminal_state,
		 core_created, core_completed, duration_ms)
		SELECT o.id, o.vendor_order_id, o.robot_id, o.station_id, o.order_type,
			o.source_node, o.delivery_node, o.vendor_state,
			o.created_at, COALESCE(o.completed_at, o.updated_at),
			EXTRACT(EPOCH FROM (COALESCE(o.completed_at, o.updated_at) - o.created_at))::BIGINT * 1000
		FROM orders o
		WHERE o.status IN ('confirmed', 'delivered', 'failed', 'cancelled')
		AND o.vendor_order_id != ''
		AND NOT EXISTS (SELECT 1 FROM mission_telemetry mt WHERE mt.order_id = o.id)`)
	return err
}

// v10OrderWaitIndex adds wait_index column to orders.
func v10OrderWaitIndex(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS wait_index INTEGER NOT NULL DEFAULT 0`)
	return err
}

// v11FixPayloadBinTypesFK fixes payload_bin_types.payload_id foreign key.
func v11FixPayloadBinTypesFK(tx *sql.Tx) error {
	return fixPayloadFK(tx, "payload_bin_types", "payload_bin_types_payload_id_fkey")
}

// v12FixPayloadManifestFK fixes payload_manifest.payload_id foreign key.
func v12FixPayloadManifestFK(tx *sql.Tx) error {
	return fixPayloadFK(tx, "payload_manifest", "payload_manifest_payload_id_fkey")
}

// v13FixNodePayloadsFK fixes node_payloads.payload_id foreign key.
func v13FixNodePayloadsFK(tx *sql.Tx) error {
	return fixPayloadFK(tx, "node_payloads", "node_payloads_payload_id_fkey")
}

// v14OrderProcessNode adds the process_node column to orders. Distinct
// from source_node — process_node names the line node a swap order
// belongs to so claimComplexBins can pick the line bin for
// order.BinID and the release-time fallback can locate the right bin.
//
// Uses ALTER TABLE ... ADD COLUMN IF NOT EXISTS so PostgreSQL itself
// enforces idempotency. Pre-2026-05 the migration did a Go-side
// schema.ColumnExists check then unconditional ALTER ADD COLUMN; if
// that check returned a stale answer (connection pool / search_path
// edge case), the migration silently no-op'd while the runner still
// recorded the version row. The plant ALN_001-class incident traced
// to this failure mode is what motivated both this DB-level
// idempotency and the transactional runner above.
func v14OrderProcessNode(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS process_node TEXT NOT NULL DEFAULT ''`)
	return err
}

// v16OrderQueueReason is Phase 4 of the bin-transit-state project.
func v16OrderQueueReason(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS queue_reason TEXT NOT NULL DEFAULT ''`)
	return err
}

// v23PendingRestocks creates the crash-safe restore-listener registry.
// The in-memory restoreRegistry would lose its entries on Core
// restart, leaving toggle-on blockers stranded in shuffle slots. The
// table is consulted at registration time, fire time, parent-
// terminal time, and at boot (re-register listeners against still-
// valid complex parents; delete stale rows).
//
// synthetic_parent_id is the OrderTypeReshuffleRestore parent row
// pre-created at scheduling time (we still create it up-front so the
// fire-time work is just "wrap the persisted plan as compound
// children of an existing parent").
func v23PendingRestocks(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS pending_restocks (
		id                    BIGSERIAL PRIMARY KEY,
		complex_parent_id     BIGINT NOT NULL,
		synthetic_parent_id   BIGINT NOT NULL,
		target_bin_id         BIGINT NOT NULL,
		expected_from_node_id BIGINT NOT NULL,
		restock_plan_json     TEXT NOT NULL,
		created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE (complex_parent_id)
	)`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS pending_restocks_target_bin_idx ON pending_restocks (target_bin_id)`)
	return err
}

// v24PendingLaneExtensions creates the persistence table for the
// lane-lock-extension listener (post-v7 cleanup). Same shape as
// pending_restocks but tighter — the lane-hold listener doesn't need
// a synthetic parent or a JSON-encoded plan; just lane ID, target
// bin ID, and the expected from-node for the race-guard at fire time.
//
// One row per active lane-hold listener; deleted on listener fire
// (bin transit), parent cancel, parent fail, and stale-row sweep at
// boot.
func v24PendingLaneExtensions(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS pending_lane_extensions (
		id                    BIGSERIAL PRIMARY KEY,
		complex_parent_id     BIGINT NOT NULL,
		lane_id               BIGINT NOT NULL,
		target_bin_id         BIGINT NOT NULL,
		expected_from_node_id BIGINT NOT NULL,
		created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE (complex_parent_id)
	)`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS pending_lane_extensions_target_bin_idx ON pending_lane_extensions (target_bin_id)`)
	return err
}

// v17UOPBinAsTruth is the consolidated migration for the UOP bin-as-
// truth refactor. The pre-production rollout cycled through six staged
// migrations (v17a-v23 in early drafts of this file) — shadow column,
// shadow table, dedup, per-station flag, then drops — that were never
// run against a production DB. Collapsed here into the final-shape
// schema so fresh installs and dev rebuilds get a clean line.
//
// Three additions:
//
//   - bin_uop_audit table. Append-only forensic log for every write
//     to bins.uop_remaining via BinManifestService and every operator
//     override / delta apply. Includes the metadata jsonb column for
//     override-row context (disposition kind, per-part diff).
//   - lineside_buckets table. Core mirror of the Edge bucket model;
//     composite UNIQUE on (station, node_id, pair_key, style_id,
//     part_number); CHECK (qty >= 0) — empty buckets are deleted
//     (Option C: location-only, active/inactive computed at query).
//   - inventory_delta_dedup table. Per-(station, scope_kind, scope_key)
//     last_seq high-water mark for at-most-once delta application.
//     Distinct from inbox_dedup (which gates order-message processing).
func v17UOPBinAsTruth(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS bin_uop_audit (
		id           BIGSERIAL PRIMARY KEY,
		bin_id       BIGINT NOT NULL,
		before_uop   INTEGER,
		after_uop    INTEGER NOT NULL,
		op           TEXT NOT NULL,
		source       TEXT NOT NULL DEFAULT '',
		order_id     BIGINT,
		payload_code TEXT NOT NULL DEFAULT '',
		actor        TEXT NOT NULL DEFAULT '',
		metadata     JSONB,
		applied_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return fmt.Errorf("create bin_uop_audit: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_bin_uop_audit_bin_time ON bin_uop_audit(bin_id, applied_at DESC)`); err != nil {
		return fmt.Errorf("index bin_uop_audit(bin_id, applied_at): %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_bin_uop_audit_op ON bin_uop_audit(op)`); err != nil {
		return fmt.Errorf("index bin_uop_audit(op): %w", err)
	}

	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS lineside_buckets (
		id BIGSERIAL PRIMARY KEY,
		station TEXT NOT NULL,
		node_id BIGINT NOT NULL,
		pair_key TEXT NOT NULL,
		style_id BIGINT NOT NULL,
		part_number TEXT NOT NULL,
		qty INTEGER NOT NULL CHECK (qty >= 0),
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE (station, node_id, pair_key, style_id, part_number)
	)`); err != nil {
		return fmt.Errorf("create lineside_buckets: %w", err)
	}
	// Round-3 Obs 8 (v21) replaces node_id with core_node_name on this
	// table. On a fresh DB, schema.Apply ran the new shape before the
	// migration pipeline started, so this index target won't exist —
	// only create the index when the legacy column is present. The
	// new-shape equivalent index gets created by v21.
	if _, err := tx.Exec(`DO $$
		BEGIN
			IF EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_name = 'lineside_buckets' AND column_name = 'node_id'
			) THEN
				CREATE INDEX IF NOT EXISTS idx_lineside_buckets_node_style ON lineside_buckets(node_id, style_id);
			END IF;
		END $$`); err != nil {
		return fmt.Errorf("index lineside_buckets(node_id, style_id): %w", err)
	}

	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS inventory_delta_dedup (
		station TEXT NOT NULL,
		scope_kind TEXT NOT NULL,
		scope_key TEXT NOT NULL,
		last_seq BIGINT NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (station, scope_kind, scope_key)
	)`); err != nil {
		return fmt.Errorf("create inventory_delta_dedup: %w", err)
	}
	return nil
}

// v15BinTransitState is Phase 1 of the bin-transit-state project. Two
// changes that together let bins move into a logical "in flight" state
// the moment a robot picks them up, freeing their source slot:
//
//  1. A single global synthetic node `_TRANSIT` (is_synthetic=true) that
//     bins occupy while the fleet is carrying them.
//
//  2. `bins.anomaly_at` timestamp column, stamped when an order fails or
//     cancels while one of its bins is still at `_TRANSIT`.
func v15BinTransitState(tx *sql.Tx) error {
	// Idempotent: if a node named `_TRANSIT` already exists with the
	// correct flag, leave it alone. If it exists with the WRONG flag,
	// fail loudly — silently accepting a hand-seeded
	// `is_synthetic=false` row would let the transit-state code
	// believe a synthetic node exists when the rest of the system
	// (lane queries, occupancy reports) treats it as a real node.
	var (
		transitID   int64
		isSynthetic bool
	)
	row := tx.QueryRow(`SELECT id, is_synthetic FROM nodes WHERE name = '_TRANSIT'`)
	if err := row.Scan(&transitID, &isSynthetic); err == nil {
		if !isSynthetic {
			return fmt.Errorf("v15 migration: _TRANSIT node id=%d exists with is_synthetic=false; "+
				"refuse to proceed (manually fix: UPDATE nodes SET is_synthetic=true WHERE id=%d)",
				transitID, transitID)
		}
	} else {
		if _, err := tx.Exec(`INSERT INTO nodes (name, is_synthetic, enabled) VALUES ('_TRANSIT', true, true)`); err != nil {
			return fmt.Errorf("create _TRANSIT node: %w", err)
		}
	}

	if _, err := tx.Exec(`ALTER TABLE bins ADD COLUMN IF NOT EXISTS anomaly_at TIMESTAMPTZ`); err != nil {
		return fmt.Errorf("add bins.anomaly_at: %w", err)
	}
	return nil
}

// fixPayloadFK checks if a payload_id FK already references payloads (no-op on fresh DBs)
// and recreates it if it still points to the old blueprints table.
func fixPayloadFK(tx *sql.Tx, table, constraintName string) error {
	var refTable string
	tx.QueryRow(`
		SELECT cc.table_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.referential_constraints rc ON rc.constraint_name = tc.constraint_name
		JOIN information_schema.constraint_column_usage cc ON cc.constraint_name = rc.unique_constraint_name
		WHERE tc.constraint_name = $1
	`, constraintName).Scan(&refTable)
	if refTable == "payloads" {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE ` + table + ` DROP CONSTRAINT IF EXISTS ` + constraintName); err != nil {
		return err
	}
	_, err := tx.Exec(`ALTER TABLE ` + table + ` ADD CONSTRAINT ` + constraintName +
		` FOREIGN KEY (payload_id) REFERENCES payloads(id) ON DELETE CASCADE`)
	return err
}
