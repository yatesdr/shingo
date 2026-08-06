package store

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

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
		// queue_code / queue_cause on orders: nullable companion columns to
		// queue_reason (the generated sentence). The baseline DDL now declares
		// them, so a fresh DB gets them from CREATE TABLE; a plant DB predating
		// them needs the pre-baseline add so the versioned migration's verify +
		// the order SELECT list (which reads both columns) don't fail ahead of
		// the migration pipeline.
		{"orders", "queue_code", `ALTER TABLE orders ADD COLUMN IF NOT EXISTS queue_code TEXT`},
		{"orders", "queue_cause", `ALTER TABLE orders ADD COLUMN IF NOT EXISTS queue_cause TEXT`},
		// origin_id / origin_class on orders: the demand grain's link from an
		// order back to the demand it served. The baseline declares both, and
		// the baseline ALSO declares idx_orders_origin_id over origin_id — an
		// index that runs inside schema.Apply, ahead of versioned migrations.
		// On a plant DB predating v59 that index would hit "column does not
		// exist" and stop startup before migration 59 ever ran. This is the
		// same shape as the misplaced code/ref index that already cost us
		// once: fine on a fresh install, broken at the plant.
		{"orders", "origin_id", `ALTER TABLE orders ADD COLUMN IF NOT EXISTS origin_id UUID`},
		{"orders", "origin_class", `ALTER TABLE orders ADD COLUMN IF NOT EXISTS origin_class TEXT NOT NULL DEFAULT ''`},
		// station_uid on edge_registry: the identity v66 introduces. THE SAME
		// SHAPE AS origin_id ABOVE, AND CAUGHT THE SAME WAY — the baseline
		// declares edge_registry_station_uid_key over the column, that index
		// runs inside schema.Apply ahead of every versioned migration, and on
		// any database predating v66 it fails with `column "station_uid" does
		// not exist` and stops startup before v66 can add it.
		//
		// VERIFIED RED. TestSchemaConvergesAcrossVintages failed on all three
		// vintages with exactly that error before this line existed. That is
		// the third time this class has been caught by that test and the
		// second time it would otherwise have reached a plant, which is why
		// the rule is written down at the head of this function rather than
		// remembered.
		//
		// ONLY station_uid, not all five of v66's columns, and the distinction
		// is the rule's actual content: this list exists for columns the
		// BASELINE ASSUMES PRESENT, not for every new column. display_name,
		// bound_instance, prev_instance and bound_at are referenced by no
		// baseline statement, so schema.Apply never touches them and v66 adds
		// them in the ordinary way. Adding them here too would work and would
		// obscure which one is load-bearing.
		{"edge_registry", "station_uid", `ALTER TABLE edge_registry ADD COLUMN IF NOT EXISTS station_uid TEXT NOT NULL DEFAULT ''`},
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
		// orders.sibling_order_id. The two legs of a two-robot swap arrive as
		// independent ComplexOrderRequests, and the second one carries the
		// first's UUID in SiblingOrderUUID; that field is the whole pairing
		// mechanism. (This used to say Edge sends a "TypeOrderSiblingLink"
		// message — no such wire type has ever existed.) Stored as the edge
		// UUID, not a resolved id FK, so arrival order doesn't matter.
		{26, "add sibling_order_uuid column to orders",
			v26OrderSiblingUUID,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "orders", "sibling_order_uuid") }},

		// v27 adds the dashboards table — saved floor-display definitions
		// for the wall-display platform (task board + three kinds since). A
		// row is a named, station-scoped view of Core's live data, served
		// chromeless at /wall-display/{id}?kiosk=1. Pure presentation config;
		// it owns no operational state, so there is no data to backfill.
		//
		// The TABLE is still `dashboards` after the round-5 page rename, and
		// deliberately: it matches the /api/.../dashboards namespace and the
		// Go type, and renaming it would leave those three disagreeing for a
		// name nobody reads off a screen.
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
		// v40: rename the replenishment enum value auto→threshold (role-aware) + swap the
		// CHECK. Once the legacy bin-count floor is retired, "auto" only ever meant
		// threshold-driven, so the model is operator|threshold. Conversion is role-aware:
		// a produce auto loader is threshold (UOP kanban autoreorder); a consume auto loader
		// (the AutoPush drain) becomes operator — consume's single mode is the window-queue
		// drain, there is no consume threshold mode. min_stock columns are left dormant.
		{40, "loader replenishment auto→threshold (role-aware) + CHECK (operator,threshold)",
			v40LoaderReplenishmentThreshold,
			v40CheckAllowsThreshold},
		// v41: home_kind discriminator on bin_loader_homes. A dedicated loader's
		// member is either a payload-pinned/unassigned HOME position or a
		// kept-partial BUFFER slot. Before this a buffer was an overloaded
		// blank-payload row — indistinguishable from an unpinned home (the D4
		// foot-gun). Additive, DEFAULT 'home' so every existing row is a home
		// (intentional buffers don't exist in prod config yet); buffers are
		// written explicitly. CHECK pins the domain.
		{41, "add home_kind to bin_loader_homes (home|buffer discriminator)",
			v41LoaderHomeKind,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "bin_loader_homes", "home_kind") }},
		// v42: dormant reservations table for the plan/apply → reservation-sourcing
		// refactor (Phase 1). Created empty and unused in Phase 0 so that Phase 1 can
		// land the write path without a flag day: the table exists on all environments
		// before any production code writes to it.
		{42, "create reservations table (dormant; Phase-1 reservation-sourcing seam)",
			v42Reservations,
			func(q schema.Querier) bool { return schema.TableExists(q, "reservations") }},
		// v43: partial unique index on reservations(bin_id) that makes Acquire
		// exactly-one-winner. Without this the Phase-1 reserve-then-confirm flow
		// has no DB-level enforcement: two concurrent Acquires on the same bin
		// could both insert pending rows and both think they won the race.
		// WHERE state IN ('pending','confirmed') leaves cancelled/expired rows
		// outside the constraint so a bin freed by Expire or Release can be
		// immediately re-reserved without violating the index.
		{43, "add uq_reservations_bin_active partial unique index (Phase-1 race gate)",
			v43ReservationsBinActiveIndex,
			func(q schema.Querier) bool {
				return schema.IndexExists(q, "uq_reservations_bin_active")
			}},
		// v44: reservation resource_kind (bin|slot|mouth) + node_id target + per-kind
		// partial unique indexes — the slot-reservation substrate. Additive and
		// dormant (bin path keeps working via the 'bin' DEFAULT); folds the schema
		// riders (state CHECK, expires_at nullable, reason column dropped). The
		// resource_kind column is the self-heal marker.
		{44, "reservations resource_kind + node_id + per-kind indexes (slot substrate)",
			v44ReservationsSlotKind,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "reservations", "resource_kind") }},

		// v45 adds orders.source_intent — the order-builder Stage-4 data home for
		// the sourcing reads that used to branch on OrderType (retrieve_empty's
		// empty-carrier intent, move's node-local sourcing, the empty-payload
		// guard). Backfills existing rows from order_type so in-flight orders keep
		// the right intent across the deploy window.
		{45, "add orders.source_intent (order-builder sourcing data home)",
			v45OrderSourceIntent,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "orders", "source_intent") }},

		// v46 adds orders.coordinated — the dispatch-provenance discriminator
		// (whether an order carries an Edge-authored multi-leg plan). It REPLACES
		// IsCoordinated's StepsJSON != "" heuristic, which becomes unsound once F1
		// persists simple plans to steps_json. Stamped at Core intake going forward;
		// backfilled from steps_json so in-flight orders keep today's exact
		// classification across the deploy window (no order changes routing).
		{46, "add orders.coordinated (dispatch-provenance discriminator)",
			v46OrderCoordinated,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "orders", "coordinated") }},

		// v47 adds orders.remaining_uop — the operator's declared release-correction
		// count, carried from intake to the single claim point. The claim moves from
		// intake to the scanner, and the scanner has no envelope; a move's
		// manifest-sync count (CreateMoveOrderWithUOP → OrderRequest.RemainingUOP)
		// therefore rides on the order row so the scanner can seed the same atomic
		// claim+sync. NULLABLE with no default: NULL is the "no sync" semantic (plain
		// claim), matching the *int nil the claim path already understands. No
		// backfill — in-flight orders already claimed at intake, so a NULL is exactly
		// right for them. Bridge column: retires when F2 carries the count in the plan.
		{47, "add orders.remaining_uop (operator release-correction count for the moved claim)",
			v47OrderRemainingUOP,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "orders", "remaining_uop") }},

		// v48 adds the structured companions to orders.queue_reason: queue_code
		// (the operator-visible category, one of protocol.QueueCode) and
		// queue_cause (the engineer-only call-site tag). queue_reason keeps
		// carrying the generated sentence — zero display regression — and the
		// two new columns let the floor/analytics GROUP BY the code instead of
		// parsing prose. Both NULLABLE with no default: NULL = a pre-schema row
		// (no backfill); a cleared reason writes NULL on both. Cause never
		// leaves Core.
		{48, "add orders.queue_code + queue_cause (structured queue-reason companions)",
			v48OrderQueueCodeCause,
			func(q schema.Querier) bool {
				return schema.ColumnExists(q, "orders", "queue_code") &&
					schema.ColumnExists(q, "orders", "queue_cause")
			}},

		// v49 creates the Core mirror for the plant-claims feed (Edge → Core
		// subject plant.claims). process_styles holds the styles a process can
		// run; style_claims holds the sourceability subset of each style's node
		// claims (node, role, swap_mode, payload, capacity, reorder). Both are
		// REPLACED per-process on every message, so a periodic full snapshot
		// rebuilds late joiners (no Kafka compaction). The dirty index the
		// recompute reads is built in code from the cache, not stored.
		{49, "create process_styles + style_claims mirror for plant-claims feed",
			v49PlantClaimsMirror,
			func(q schema.Querier) bool {
				return schema.TableExists(q, "process_styles") &&
					schema.TableExists(q, "style_claims")
			}},

		// v50 adds the advanced load sequence surface: a nullable
		// advanced_load_sequence NAME on payloads (the switch — empty/NULL =
		// today's single load block, byte-identical) and the load_sequences
		// registry (sequence name → ordered binTask-name list). The registry is
		// editable data, not constants: a plant names its RDS-side binTask keys
		// differently, so the task list lives in a table. Seeded with one row
		// (Child cart interlock → the four-name sequence from the evidence doc).
		{50, "add payloads.advanced_load_sequence + load_sequences registry",
			v50AdvancedLoadSequence,
			func(q schema.Querier) bool {
				return schema.ColumnExists(q, "payloads", "advanced_load_sequence") &&
					schema.TableExists(q, "load_sequences")
			}},
		// ⚠ NUMBERING COLLISION AHEAD — READ BEFORE REBASING THE LANE CAMPAIGN.
		//
		// v51 is taken HERE, on main. The unpushed lane campaign (refactor-phase1,
		// held back per the Springfield merge brief §5) also carries a v51 and a
		// v52 — the durable lane rows and the pending_restocks drop. Those two
		// MUST be renumbered to v52 and v53 when that branch rebases onto main.
		// The migration list is keyed by integer, so two v51s do not conflict at
		// compile time: the second one silently never runs against a database
		// that already recorded 51, and the schema diverges per-plant depending
		// on which build reached it first. Renumber at rebase; do not merge the
		// campaign without checking this line.
		{51, "add process_styles.is_active (running style from the plant-claims feed)",
			v51ProcessStyleActive,
			func(q schema.Querier) bool {
				return schema.ColumnExists(q, "process_styles", "is_active")
			}},
		// v52: R1 shadow read-model. Edge's periodic per-consuming-node lineside
		// on-hand, its OWN table (NOT bins), so Core can compute the monitor's
		// lineside term both ways (ledger vs Edge reports) and log
		// firing-decision disagreements — SHADOW, deciding off the ledger.
		//
		// ⚠ NUMBERING: v52 is taken HERE on branch monitor-collapse-r1. Sibling
		// lanes E and G (this round) may also add migrations, and the unpushed
		// lane campaign already carries a v51/v52 to renumber (see the v51
		// collision note above). This migration is a pure additive CREATE TABLE
		// with no dependency on any constraint, so it renumbers trivially on
		// merge — order it after any dedup/data migration if one is added.
		{52, "add edge_lineside_reports (R1 shadow read-model for the lineside term)",
			v52EdgeLinesideReports,
			func(q schema.Querier) bool { return schema.TableExists(q, "edge_lineside_reports") }},
		{53, "backfill mission_telemetry.robot_id from orders (was written blank)",
			v53BackfillTelemetryRobotID, nil},
		{54, "backfill mission_events.robot_id from orders (the other half of 53)",
			v54BackfillMissionEventRobotID, nil},
		{55, "order_history += code, actor, ref (the reason, typed, pointing at what it concerns)",
			v55OrderHistoryReasonColumns,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "order_history", "ref") }},
		{56, "sourceability_events — persist the verdict changes the monitor already computes",
			v56SourceabilityEvents,
			func(q schema.Querier) bool { return schema.TableExists(q, "sourceability_events") }},
		{57, "rename legacy downtime_events partitions to the aligned form",
			v57RenameDowntimePartitions, nil},
		// FOUND BY TestSchemaConvergesAcrossVintages, and the first thing it
		// found. A database installed before 2026-05-21 carries
		// DEFAULT '' on lineside_buckets.core_node_name; one installed after
		// does not, because the baseline CREATE TABLE declares the column
		// without one.
		//
		// The default was never intent — v21's own comment says it exists "so
		// existing orphaned rows don't break the column add", which is a
		// mechanical requirement of ADD COLUMN NOT NULL on a populated table.
		// It then stayed forever.
		//
		// It matters because '' is the UNKNOWN value for this column and the
		// table has UNIQUE (station, core_node_name, pair_key, style_id,
		// part_number). At a plant, an insert that omitted the node would
		// silently write '' and collide unrelated nodes' buckets; on a fresh
		// install the same insert errors. Every insert site supplies the
		// column, so dropping the default takes nothing away.
		{58, "drop lineside_buckets.core_node_name's DEFAULT (converge aged DBs with fresh)",
			v58DropLinesideCoreNodeNameDefault,
			func(q schema.Querier) bool { return !columnHasDefault(q, "lineside_buckets", "core_node_name") }},
		// The demand grain. demand_origins is Core's history of every episode;
		// orders gains the link back to the demand it served.
		//
		// The verify checks the LAST thing this migration creates, not the
		// first. Everything here runs in one transaction (runOneMigration), so
		// a partial apply is not reachable — but a post-condition that passes
		// while the tail is missing would be a self-heal that never heals, and
		// which end it checks costs nothing to get right.
		{59, "demand_origins + orders.origin_id/origin_class (the demand grain)",
			v59DemandOrigins,
			func(q schema.Querier) bool {
				return schema.TableExists(q, "demand_origins") &&
					schema.ColumnExists(q, "orders", "origin_class")
			}},
		// WHICH MECHANISM CLOSED IT. The reconciling sweep deliberately uses the
		// SAME close_reason codes as the notification paths, so the surface does
		// not grow a second vocabulary for the same facts about the plant. The
		// cost of that choice is that a silent failure of every notification
		// path looks identical to a healthy system — the sweep quietly picks up
		// all the work and every surface stays green. closed_by makes the
		// sweep's share of the work a number somebody can look at.
		//
		// Separate migration rather than an edit to 59: 59 is pushed, and a
		// migration anyone may already have run is not a file you go back and
		// change.
		{60, "demand_origins.closed_by (which mechanism ended the episode)",
			v60DemandOriginClosedBy,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "demand_origins", "closed_by") }},
		// AGING IS A TIMESTAMP, NOT A FOURTH CLASS. origin_class answers HOW
		// DID THIS ORDER RELATE TO A DEMAND — attached, structurally
		// originless, or it should have had an episode and didn't. That is a
		// create-time fact and it is true forever. The reconciling sweep was
		// overwriting it with `orphan_aged`, which leaves the row unable to
		// answer what it was classified as when it was created: a FACT
		// OVERWRITTEN BY A DERIVATION, the uopCache mistake in a fourth
		// costume.
		//
		// It also matches the shape this schema already uses everywhere for
		// exactly this — closed_at beside close_reason, anomaly_at beside the
		// bin: a nullable timestamp NEXT TO the fact, never a state value
		// inside it.
		//
		// AN AGED ORPHAN IS STILL A FINDING. Aging changes which lane it sits
		// in and who is expected to act on it, not whether it is a problem.
		// Never deleted, never auto-attached.
		{61, "orders.orphan_aged_at (aging is a timestamp, not a fourth origin_class)",
			v61OrphanAgedAt,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "orders", "orphan_aged_at") }},
		// THE DRAWN NETWORK BECOMES THE DRIVEN NETWORK. A scene edge is a lane
		// the fleet actually drives, and SEER states its shape with two
		// cubic-Bezier handles that shingo discarded at decode. The map drew
		// the chord instead, which at Springfield puts the painted lane up to
		// 1.30 m from the real one (LM10-LM113, a 7.17 m aisle), so robots
		// visibly leave the network as painted, and the route lengths derived
		// from those segments run up to 24% short.
		//
		// FOUR NULLABLE COLUMNS, NOT FOUR ZEROED ONES. The origin is a real
		// coordinate on a plant map; a straight aisle whose handles defaulted
		// to (0,0) would render sweeping tens of metres across the floor. NULL
		// is the only value that means "this lane has no bend", and it must be
		// distinguishable from a bend that happens to pass near (0,0).
		//
		// SINGLE-HOMED HERE, NOT IN THE BASELINE — B9. Nothing indexes these,
		// so they carry none of the claim orders.origin_id has, and an ALTER
		// runs identically on the fresh and aged convergence paths.
		//
		// No backfill. Scene sync deletes and re-inserts every area on each
		// pass, so the handles arrive on the first sync after deploy. Until
		// then every row reads NULL, which renders as today's straight line —
		// the pre-migration behaviour exactly, not a wrong bend.
		{62, "scene_edges control handles (draw the lane the robot drives)",
			v62SceneEdgeControlHandles,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "scene_edges", "ctrl2_y") }},
		// TWO IDENTITY SYSTEMS FOR ONE SET OF PROCESSES, RECONCILED.
		//
		// v59 declared demand_origins.process_id as BIGINT, holding an Edge
		// SQLite row id. The DEPLOYED process_styles.process_id and
		// style_claims.process_id are TEXT, holding the Edge process NAME
		// ("SNF2") — as is PlantClaimsReport.ProcessID on the wire. So Core held
		// two descriptions of the same processes with no query able to put them
		// side by side, which killed two Phase 6 designs outright.
		//
		// FREE NOW, EXPENSIVE LATER, and that is the whole reason it is here.
		// v59 is above every plant's applied version, so no plant has this table
		// and no row is being rewritten anywhere. After the first plant runs v59
		// this stops being a column type and becomes a data migration on live
		// forensic history.
		//
		// A NEW MIGRATION RATHER THAN AN EDIT TO v59, for the reason v60 already
		// records on itself: v59 is pushed, and a migration anyone may already
		// have run is not a file you go back and change. A fresh database
		// therefore creates the column as BIGINT and converts it one step later,
		// which is the same two-step v58 and v61 take.
		{63, "demand_origins.process_id BIGINT -> TEXT (one identity for a process, not two)",
			v63DemandOriginProcessIDText,
			func(q schema.Querier) bool {
				return schema.ColumnType(q, "demand_origins", "process_id") == "text"
			}},
		// THE STATEMENT THAT HANDLES A DUPLICATE EDGE IS THE STATEMENT THAT
		// DESTROYS THE EVIDENCE OF ONE.
		//
		// edge_registry.station_id is UNIQUE and Register's upsert explicitly
		// sets `hostname = excluded.hostname` on conflict. So two Pis configured
		// with the same station id never collide: they take turns owning one
		// row, each register overwriting the other's hostname, and nothing
		// anywhere records that a second machine exists. Today's Edge defaults
		// (namespace `plant-a`, line `line-1`) guarantee that shape on the
		// second Pi, and install-edge.sh writes a config carrying only
		// database_path — so a fresh install cannot come up as anything else.
		//
		// bound_hostname holds the FIRST hostname to register a station and is
		// never reassigned by a register. The conflict_* three record the most
		// recent mismatching claimant, when, and how many there have been.
		//
		// THE BACKFILL IS WHAT MAKES THIS SILENT AT THE PLANTS. Setting
		// bound_hostname = hostname for the existing rows binds each live plant
		// to the box it is already running on, so the first register after
		// deploy matches and nothing fires. A DEFAULT '' with no backfill would
		// instead have the first register CLAIM the lease, which is the same end
		// state one register later — but it would leave a window where a swap
		// during the deploy silently rebound the station, and the whole point is
		// that no rebind is silent.
		//
		// DETECTOR, NOT GATE — deliberately, and the reason is in the signal
		// rather than in caution. A differing hostname is equally the signature
		// of a legitimate box replacement, which Core cannot presently
		// distinguish because it has no enrollment step to ask about. Refusing
		// on it would turn a reimaged Pi into a plant with no Edge.
		{64, "edge_registry bound_hostname + conflict_* (two machines, one station id)",
			v64EdgeRegistryHostnameBinding,
			func(q schema.Querier) bool {
				return schema.ColumnExists(q, "edge_registry", "bound_hostname") &&
					schema.ColumnExists(q, "edge_registry", "conflict_at")
			}},
		// PREREQUISITE of per-edge identity. `station` is one-valued today, so
		// this merges nothing; it stops being one-valued the moment each edge
		// gets its own id, and then a station-scoped write against a
		// station-blind SUM double-counts physical inventory. See the function
		// comment for why the repo has already made this mistake in this key.
		{65, "lineside_buckets: drop station from the uniqueness key (identity prerequisite)",
			v65LinesideBucketsDropStationFromKey,
			func(q schema.Querier) bool {
				var n int
				if err := q.QueryRow(`
					SELECT count(*) FROM pg_constraint con
					JOIN pg_class rel ON rel.oid = con.conrelid
					WHERE rel.relname = 'lineside_buckets'
					  AND con.contype = 'u'
					  AND pg_get_constraintdef(con.oid) LIKE '%station%'`).Scan(&n); err != nil {
					return false
				}
				return n == 0
			}},
		// EDGE IDENTITY. Backfilling station_uid = station_id is what makes
		// this deployable ahead of the Edge that knows about uids: for one
		// window the uid and the legacy routing string are the same characters,
		// so a legacy Edge's register resolves against the uid key unchanged.
		// See the function comment — the compatibility is in the data, not in a
		// branch, which is why nothing has to be unwound later.
		{66, "edge_registry: station_uid + display_name + binding lease; line_ids retired",
			v66EdgeIdentity,
			func(q schema.Querier) bool {
				return schema.ColumnExists(q, "edge_registry", "station_uid") &&
					schema.ColumnExists(q, "edge_registry", "bound_at") &&
					!schema.ColumnExists(q, "edge_registry", "line_ids")
			}},
		{67, "edge_registry.claimed_at — an edge may introduce itself; a human says what it is",
			v67EdgeClaim,
			func(q schema.Querier) bool {
				return schema.ColumnExists(q, "edge_registry", "claimed_at")
			}},
		{68, "supply_refusals — a person's statement that they cannot supply a call",
			v68SupplyRefusals,
			func(q schema.Querier) bool {
				return schema.TableExists(q, "supply_refusals")
			}},
		// THE INDEX SPRINGFIELD ALREADY HAS, WRITTEN DOWN.
		//
		// A `\d orders` at Springfield (2026-08-02) returns a UNIQUE index on
		// edge_uuid, partial on `edge_uuid <> ''`. Nothing in this repository
		// creates it: postgres_ddl.go and the schema snapshot both declare a
		// PLAIN index, and no migration touches it. So it was applied by hand,
		// and a fresh install gets a database the plant's schema does not match —
		// the exact drift the DDL constants exist to prevent.
		//
		// PARTIAL, and that is not a stylistic choice. Springfield holds 23 rows
		// with an empty edge_uuid (all of them cancelled store orders from April,
		// from the door that wrote a row before the planner rejected it). A plain
		// unique index would fail outright on those rows. The plant has already
		// proved which form works.
		//
		// Numbered 71 deliberately: the lane campaign on refactor-phase1 runs up
		// to 70, so this cannot collide with it whichever lands first. See the
		// numbering warning above v51.
		{71, "orders.edge_uuid unique (partial) — match the index the plants already run",
			v71OrdersUUIDUnique,
			func(q schema.Querier) bool { return uuidIndexIsUnique(q) }},
		// A DATA migration, not a shape one: no ALTER, so no matching edit in
		// the DDL constants. It moves the value, not the column.
		{72, "core-spot becomes core-operator, in every table that stores it",
			v72StationCoreOperator,
			func(q schema.Querier) bool { return noCoreSpotLeft(q) }},
		{73, "orders.edge_uuid unique — exempt the restore parent's derived name",
			v73OrdersUUIDUniqueExemptRestore,
			func(q schema.Querier) bool { return uuidIndexExemptsRestore(q) }},
		// v74: whether a shared-window loader spreads its inbound empties across
		// its windows, or funnels them to the first one, becomes a property of the
		// loader instead of a plant-wide Edge config key. DEFAULT FALSE = spread,
		// which is what the Edge flag already resolved to when unset — so every
		// existing loader keeps behaving exactly as it does today. Inert for
		// dedicated loaders: their positions never shared a budget to begin with.
		{74, "add funnel_windows to bin_loaders (per-loader window spreading)",
			v74LoaderFunnelWindows,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "bin_loaders", "funnel_windows") }},
		// v75: what carriers a loader wants, and which of them each window can
		// physically take. Two tables because they answer two different
		// questions — see v75LoaderCarrierMix. Both start EMPTY, and empty means
		// exactly today's behaviour: no declared mix, every window takes
		// anything.
		{75, "add the loader carrier mix (per-loader quota, per-window capability)",
			v75LoaderCarrierMix,
			func(q schema.Querier) bool { return schema.TableExists(q, "bin_loader_quotas") }},
		// v77, not v76: the lane-occupancy migration on reshuffling-work holds
		// 76 and is unmerged, so this skips it rather than racing it. The gap
		// is intentional and harmless — this is a list, not a range. Whoever
		// merges 76 must insert it ABOVE this entry: latestMigrationVersion is
		// read off the LAST element, so appending 76 after 77 would silently
		// walk the reported head version backwards.
		{77, "collect robot localization confidence (samples, low trail, daily roll-ups)",
			v77RobotConfidence,
			func(q schema.Querier) bool { return schema.TableExists(q, "robot_confidence_samples") }},
		// 78, not 76: see the numbering note above v77 — 76 is still held by
		// the unmerged lane-occupancy migration on reshuffling-work.
		{78, "carry the robot's advanced-area membership onto every confidence sample",
			v78ConfidenceAreaIDs,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "robot_confidence_samples", "area_ids") }},
		// 79 is the next free number ON THIS BRANCH (main runs 1-68, 71-75,
		// 77; 78 is the area_ids commit above). It is NOT a number to copy
		// out of a design document: 76 and a SECOND 77 are held by the
		// unmerged reshuffling-work branch, so whoever merges resolves this
		// against whatever actually landed first.
		{79, "carry the robot's map hash and active alarms onto every confidence sample",
			v79ConfidenceMapAndAlarms,
			func(q schema.Querier) bool { return schema.ColumnExists(q, "robot_confidence_samples", "map_md5") }},
		{80, "confidence aggregates key on the physical lane, and keep both populations",
			v80LaneConfidenceDaily,
			func(q schema.Querier) bool { return schema.TableExists(q, "lane_confidence_daily") }},
		{81, "version the scene: a map edit becomes an event with a magnitude",
			v81SceneVersioning,
			func(q schema.Querier) bool { return schema.TableExists(q, "scene_lane_versions") }},
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

func v45OrderSourceIntent(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS source_intent TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	// Backfill from order_type so in-flight orders keep the right sourcing intent
	// across the deploy window; new orders are stamped at intake. Mirrors
	// dispatch.SourceIntentForType exactly: retrieve_empty→empty, move→local,
	// everything else (retrieve, store, complex, …)→'' (full/default). Store is
	// deliberately NOT 'local' — it self-sources and must stay non-exempt from
	// the scanner payload guard.
	_, err := tx.Exec(`UPDATE orders SET source_intent = CASE order_type
		WHEN 'retrieve_empty' THEN 'empty'
		WHEN 'move' THEN 'local'
		ELSE '' END`)
	return err
}

func v47OrderRemainingUOP(tx *sql.Tx) error {
	// NULLABLE, no default: NULL = "no sync" (plain claim), a positive value syncs
	// the bin manifest at the (now scanner-side) claim. No backfill — in-flight
	// orders claimed at intake before this deploy, so NULL is correct for them.
	_, err := tx.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS remaining_uop INTEGER`)
	return err
}

func v46OrderCoordinated(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS coordinated BOOLEAN NOT NULL DEFAULT false`); err != nil {
		return err
	}
	// Backfill = (steps_json != '') — reproduces IsCoordinated's pre-column
	// semantics EXACTLY (StepsJSON != "" ⟺ coordinated), so no in-flight order
	// changes its dispatch tail across the deploy. New orders are stamped at intake
	// (complex intake → true, everything else → false). Idempotent: a re-run
	// recomputes the same value; steps_json is NOT NULL DEFAULT '' so the guard is
	// just defensive.
	_, err := tx.Exec(`UPDATE orders SET coordinated = (steps_json IS NOT NULL AND steps_json != '')`)
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

// v40LoaderReplenishmentThreshold renames the replenishment enum value auto→threshold
// (role-aware) and swaps the CHECK constraint. Order matters: the old CHECK only allows
// ('auto','operator'), so an UPDATE to 'threshold' would violate it — drop the CHECK
// FIRST, convert, then add the new CHECK. Idempotent: after running there are no 'auto'
// rows and the new CHECK is in place; re-running is a no-op (no rows match, DROP/ADD IF).
func v40LoaderReplenishmentThreshold(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE bin_loaders DROP CONSTRAINT IF EXISTS bin_loaders_replenishment_check`,
		`UPDATE bin_loaders SET replenishment='threshold' WHERE replenishment='auto' AND role='produce'`,
		`UPDATE bin_loaders SET replenishment='operator'  WHERE replenishment='auto' AND role='consume'`,
		`ALTER TABLE bin_loaders ADD CONSTRAINT bin_loaders_replenishment_check CHECK (replenishment IN ('operator','threshold'))`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v40 replenishment auto→threshold: %w", err)
		}
	}
	return nil
}

// v40CheckAllowsThreshold is the v40 post-condition: the replenishment CHECK constraint
// now references 'threshold'. Reads the constraint def from the catalog.
func v40CheckAllowsThreshold(q schema.Querier) bool {
	var def string
	if err := q.QueryRow(`SELECT COALESCE(string_agg(pg_get_constraintdef(oid), ' '), '')
		FROM pg_constraint WHERE conrelid='bin_loaders'::regclass AND contype='c'`).Scan(&def); err != nil {
		return false
	}
	return strings.Contains(def, "threshold")
}

// v41LoaderHomeKind adds the home/buffer discriminator to bin_loader_homes so a
// dedicated loader's BUFFER slot (kept partials, no pinned payload) is no longer
// the same blank-payload row as an unpinned HOME position. Additive + idempotent;
// DEFAULT 'home' backfills every existing row as a home (no live buffer config
// exists yet). The CHECK pins the two-value domain.
func v41LoaderHomeKind(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE bin_loader_homes ADD COLUMN IF NOT EXISTS home_kind TEXT NOT NULL DEFAULT 'home'`,
		`ALTER TABLE bin_loader_homes DROP CONSTRAINT IF EXISTS bin_loader_homes_home_kind_check`,
		`ALTER TABLE bin_loader_homes ADD CONSTRAINT bin_loader_homes_home_kind_check CHECK (home_kind IN ('home','buffer'))`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v41 home_kind: %w", err)
		}
	}
	return nil
}

// v42Reservations creates the reservations table for the Phase-1
// reservation-sourcing seam. Dormant in Phase 0: no production code
// writes to it. store/reservations exposes the Acquire/Confirm/Release/Expire
// surface as stubs; Phase 1 fills the bodies and wires callers.
func v42Reservations(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS reservations (
		id          BIGSERIAL PRIMARY KEY,
		order_id    BIGINT NOT NULL REFERENCES orders(id),
		bin_id      BIGINT NOT NULL REFERENCES bins(id),
		state       TEXT NOT NULL DEFAULT 'pending',
		reserved_by TEXT NOT NULL DEFAULT '',
		reason      TEXT NOT NULL DEFAULT '',
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		expires_at  TIMESTAMPTZ NOT NULL
	)`); err != nil {
		return fmt.Errorf("create reservations table: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_reservations_order ON reservations(order_id)`); err != nil {
		return fmt.Errorf("create idx_reservations_order: %w", err)
	}
	_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_reservations_bin ON reservations(bin_id)`)
	return err
}

// v43ReservationsBinActiveIndex adds the partial unique index that makes
// Phase-1 Acquire exactly-one-winner. Two concurrent Acquires on the same
// bin both try to INSERT a 'pending' row; only one can satisfy the uniqueness
// constraint and the other gets a conflict, returning the race signal.
// WHERE state IN ('pending','confirmed') leaves expired/released rows
// outside the constraint so a freed bin can be immediately re-reserved.
func v43ReservationsBinActiveIndex(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_reservations_bin_active
		ON reservations (bin_id) WHERE state IN ('pending','confirmed')`)
	return err
}

// v44ReservationsSlotKind is the slot-reservation substrate: a resource_kind
// discriminator (bin|slot|mouth) + a nullable node_id target + per-kind partial
// unique indexes, so a slot reservation is a soft row keyed on node_id exactly as
// a bin reservation is keyed on bin_id. Additive and DORMANT — no code reads or
// writes slot/mouth rows yet (the bin path keeps working via resource_kind's 'bin'
// DEFAULT, which backfills every existing row). NO mouth index (the schema accepts
// a mouth row, but its granularity is the lane phase's call).
//
// It also folds the schema riders: a `state` domain CHECK (a typo previously
// ESCAPED the partial index silently), expires_at made nullable (retired as a
// reaping key — owner-liveness reaping keys on order liveness), and the
// always-” `reason` column dropped.
//
// NOTE ON THE PARTIAL-INDEX PREDICATES: `state IN ('pending','confirmed')` covers
// EVERY row today — release is a hard DELETE (reservations.Release/ReleaseBy*),
// never a state transition to a terminal value, so no released rows linger. The
// predicate is kept future-proof (and is now redundant-but-harmless alongside the
// state CHECK); do not read it as evidence that other states exist on disk.
func v44ReservationsSlotKind(tx *sql.Tx) error {
	stmts := []string{
		// Discriminator; DEFAULT 'bin' backfills existing rows to the bin kind.
		`ALTER TABLE reservations ADD COLUMN IF NOT EXISTS resource_kind TEXT NOT NULL DEFAULT 'bin'`,
		// Slot/mouth target; nullable (a bin row leaves it NULL). RESTRICT FK: a
		// reserved node cannot be admin-deleted out from under the hold (documented).
		`ALTER TABLE reservations ADD COLUMN IF NOT EXISTS node_id BIGINT REFERENCES nodes(id)`,
		// bin_id is no longer mandatory — slot/mouth rows carry node_id instead.
		`ALTER TABLE reservations ALTER COLUMN bin_id DROP NOT NULL`,
		// expires_at retired as a reaping key; nullable and no longer written.
		`ALTER TABLE reservations ALTER COLUMN expires_at DROP NOT NULL`,
		// reason was always '' — drop it.
		`ALTER TABLE reservations DROP COLUMN IF EXISTS reason`,
		// Kind domain.
		`ALTER TABLE reservations DROP CONSTRAINT IF EXISTS reservations_resource_kind_check`,
		`ALTER TABLE reservations ADD CONSTRAINT reservations_resource_kind_check CHECK (resource_kind IN ('bin','slot','mouth'))`,
		// Exactly-one-of the target columns, keyed by kind.
		`ALTER TABLE reservations DROP CONSTRAINT IF EXISTS reservations_kind_target_check`,
		`ALTER TABLE reservations ADD CONSTRAINT reservations_kind_target_check CHECK (
			(resource_kind = 'bin' AND bin_id IS NOT NULL AND node_id IS NULL)
			OR (resource_kind IN ('slot','mouth') AND node_id IS NOT NULL AND bin_id IS NULL))`,
		// State domain CHECK.
		`ALTER TABLE reservations DROP CONSTRAINT IF EXISTS reservations_state_check`,
		`ALTER TABLE reservations ADD CONSTRAINT reservations_state_check CHECK (state IN ('pending','confirmed'))`,
		// Rescope the bin active-uniqueness index to bin rows only.
		`DROP INDEX IF EXISTS uq_reservations_bin_active`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_reservations_bin_active
			ON reservations (bin_id) WHERE resource_kind='bin' AND state IN ('pending','confirmed')`,
		// Per-node active-uniqueness for slot rows — the slot dual of the bin index.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_reservations_slot_active
			ON reservations (node_id) WHERE resource_kind='slot' AND state IN ('pending','confirmed')`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v44 reservations slot kind: %w", err)
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
// belongs to so ApplyComplexPlan can pick the line bin for
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

// v48OrderQueueCodeCause adds the structured companions to orders.queue_reason.
// See the migration-list comment for v48. Both columns are nullable (NULL = a
// pre-schema row, or a cleared reason); no default, no backfill — existing rows
// keep NULL and read as "uncoded" until the next time the order is parked.
func v48OrderQueueCodeCause(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE orders ADD COLUMN IF NOT EXISTS queue_code TEXT`,
		`ALTER TABLE orders ADD COLUMN IF NOT EXISTS queue_cause TEXT`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v48 orders queue_code/queue_cause: %w", err)
		}
	}
	return nil
}

// v49PlantClaimsMirror creates the two Core mirror tables for the
// plant-claims feed (Edge → Core subject plant.claims). process_styles is one
// row per (process, style) Edge reports; style_claims is one row per
// sourceability-relevant claim under that (process, style). Both are owned by
// the feed: the handler DELETEs then re-INSERTs for a process on every
// message, so the tables are a pure mirror of Edge's plant spec at a point in
// time. config_gen lets the handler reject a stale (older) snapshot for a
// process when a newer one already landed.
//
// No foreign keys to Edge-owned entities: process_id/style_id are opaque
// Edge identifiers (names), mirrored verbatim. The dirty index the
// recompute consumes (payload_code → set of (process, style)) is derived in
// code from style_claims, not stored as a table.
func v49PlantClaimsMirror(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS process_styles (
			process_id   TEXT NOT NULL,
			style_id     TEXT NOT NULL,
			config_gen   BIGINT NOT NULL DEFAULT 0,
			updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (process_id, style_id)
		)`,
		`CREATE TABLE IF NOT EXISTS style_claims (
			process_id          TEXT NOT NULL,
			style_id            TEXT NOT NULL,
			core_node_name      TEXT NOT NULL,
			role                TEXT NOT NULL,
			swap_mode           TEXT NOT NULL,
			payload_code        TEXT NOT NULL DEFAULT '',
			allowed_payload_codes TEXT NOT NULL DEFAULT '[]',
			uop_capacity        INTEGER NOT NULL DEFAULT 0,
			reorder_point       INTEGER NOT NULL DEFAULT 0,
			seq                 INTEGER NOT NULL DEFAULT 0
		)`,
		// Dirty-index lookup: which (process, style) rows need recompute when a
		// payload changes. The primary payload is indexed; the allowed set is a
		// JSON array the handler scans in code for the secondary matches.
		`CREATE INDEX IF NOT EXISTS idx_style_claims_payload
			ON style_claims (payload_code)`,
		`CREATE INDEX IF NOT EXISTS idx_style_claims_process_style
			ON style_claims (process_id, style_id)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v49 plant-claims mirror: %w", err)
		}
	}
	return nil
}

// v50AdvancedLoadSequence adds the advanced load sequence surface. Two parts:
//
//  1. payloads.advanced_load_sequence — a nullable NAME field. Empty/NULL =
//     today's single load block (byte-identical, no behavior change). A name
//     set selects a configured binTask-name sequence from load_sequences; the
//     dispatch path expands the load leg into one same-location block per name.
//
//  2. load_sequences — the editable registry: one row per sequence name, with
//     task_names a JSON array of binTask names in execution order. Data, not
//     constants — a plant names its RDS-side binTask keys differently, so the
//     list lives in a table an engineer edits, not in code. Seeded with the one
//     sequence the quarter-child-cart feature needs (Child cart interlock → the
//     four-name order from the working Postman order in the evidence doc).
//
// The seed is INSERT ... ON CONFLICT DO NOTHING so re-running is a no-op and an
// engineer's edits to the seeded row are never clobbered.
func v50AdvancedLoadSequence(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE payloads ADD COLUMN IF NOT EXISTS advanced_load_sequence TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE IF NOT EXISTS load_sequences (
			name        TEXT PRIMARY KEY,
			task_names  TEXT NOT NULL DEFAULT '[]',
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		// Seed the one sequence the feature ships with. task_names is a JSON
		// array in execution order; the plant may rename any key, so the seeded
		// values are a starting point, not authoritative.
		`INSERT INTO load_sequences (name, task_names) VALUES
			('Child cart interlock', '["Go_AP1","Spin_90","load","Spin_inverse_90"]')
		 ON CONFLICT (name) DO NOTHING`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v50 advanced load sequence: %w", err)
		}
	}
	return nil
}

// v51ProcessStyleActive marks which mirrored style a process is running.
//
// Core had no notion of a running style at all: the plant-claims feed carried a
// process's styles and their claims but no active flag, so the sourcing page
// could only say what a process COULD change over to, never what it is on now.
// Edge has known all along — processes.active_style_id, the same field Edge
// resolves node claims through — it simply never crossed the wire.
//
// Additive and idempotent: NOT NULL DEFAULT FALSE, so every existing row reads
// "not the active style" until the next plant-claims snapshot lands (Edge
// republishes on every spec change, on a timer, and at boot, so that is minutes
// at worst). A partial index rather than a UNIQUE constraint: at most one style
// per process should be active, but a mid-flight snapshot is replaced
// wholesale inside one transaction, and a hard constraint would turn a
// transient double-active during replace into a failed mirror write.
func v51ProcessStyleActive(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE process_styles ADD COLUMN IF NOT EXISTS is_active BOOLEAN NOT NULL DEFAULT FALSE`,
		`CREATE INDEX IF NOT EXISTS ix_process_styles_active
			ON process_styles (process_id) WHERE is_active`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v51 process_styles.is_active: %w", err)
		}
	}
	return nil
}

// v52EdgeLinesideReports creates the R1 shadow read-model table: Edge's
// periodic per-consuming-node lineside on-hand. One row per
// (station, core_node_name, payload_code), upserted on each 60s report. Core
// reads the fresh (< 3 min) rows to shadow the monitor's lineside term against
// the ledger and log firing-decision disagreements. Its OWN table — NOT bins —
// and nothing here writes bins.uop_remaining; the delta path stays that
// column's only writer.
func v52EdgeLinesideReports(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS edge_lineside_reports (
		station         TEXT NOT NULL,
		core_node_name  TEXT NOT NULL,
		payload_code    TEXT NOT NULL,
		bin_count       INTEGER NOT NULL DEFAULT 0,
		bin_uop         INTEGER NOT NULL DEFAULT 0,
		bucket_qty      INTEGER NOT NULL DEFAULT 0,
		reported_at     TIMESTAMPTZ NOT NULL,
		updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (station, core_node_name, payload_code)
	)`); err != nil {
		return fmt.Errorf("v52 edge_lineside_reports: %w", err)
	}
	return nil
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

// v53BackfillTelemetryRobotID repairs summary rows written with a blank
// robot_id.
//
// finalizeMissionTelemetry took the robot from the status-change event
// rather than from the order it had already loaded, and terminal
// transitions frequently carry no robot on the event. Every affected row
// is invisible to the per-robot breakdown and to the robot filter, so the
// history stays broken even after the writer is fixed unless it is
// backfilled. orders.robot_id is the authoritative value the writer should
// have used, so copy it across.
//
// Deliberately not verify-gated: the post-condition ("no blank robot_id")
// is legitimately unreachable for orders that genuinely never had a robot
// assigned, so a verify would re-run this forever.
func v53BackfillTelemetryRobotID(tx *sql.Tx) error {
	_, err := tx.Exec(`UPDATE mission_telemetry mt
		SET robot_id = o.robot_id
		FROM orders o
		WHERE mt.order_id = o.id
		  AND mt.robot_id = ''
		  AND o.robot_id <> ''`)
	return err
}

// v54BackfillMissionEventRobotID is the other half of 53.
//
// 53 repaired mission_telemetry and the summary writer. The per-transition
// writer immediately above it in the same file — recordMissionEvent — kept
// taking the robot from the EVENT, so mission_events.robot_id stayed blank on
// every transition where the vendor carried no vehicle. That is all sim
// DriveState calls and most terminal transitions, which is most of the table.
//
// The blank id is not the only casualty: recordMissionEvent gates its robot
// position snapshot on the same value, so those rows also have NULL
// robot_x/y/angle/battery/station. Those cannot be backfilled — the position
// was a point-in-time cache read and is gone. This recovers the identity,
// which is what the per-robot breakdown and the robot filter need.
//
// Not verify-gated, same reasoning as 53: "no blank robot_id" is legitimately
// unreachable for orders that never had a robot, so a verify would re-run
// this forever.
// v57RenameDowntimePartitions renames downtime_events_y2026m07 to
// downtime_events_2026_07, aligning with store/heartbeat's cell_part_events
// form.
//
// This is not cosmetic. createMonthPartition now emits the aligned name, and
// CREATE TABLE IF NOT EXISTS guards the NAME, not the RANGE — so on any
// database carrying legacy-named partitions the new one fails with
//
//	partition "downtime_events_2026_07" would overlap partition
//	"downtime_events_y2026m07" (SQLSTATE 42P17)
//
// and the month has no usable partition. Caught on the houseserver sim,
// 2026-07-25, which is the only place downtime_events has ever had rows.
//
// A rename is metadata-only: no data moves, no locks beyond the catalog
// update, and the partition keeps its bounds and its attachment.
func v57RenameDowntimePartitions(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'downtime_events'
		  AND c.relname ~ '^downtime_events_y[0-9]{4}m[0-9]{2}$'`)
	if err != nil {
		return fmt.Errorf("v57 list legacy partitions: %w", err)
	}
	var legacy []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return fmt.Errorf("v57 scan partition name: %w", err)
		}
		legacy = append(legacy, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("v57 iterate partitions: %w", err)
	}

	for _, old := range legacy {
		// downtime_events_yYYYYmMM -> downtime_events_YYYY_MM
		year := old[len("downtime_events_y") : len("downtime_events_y")+4]
		month := old[len(old)-2:]
		renamed := fmt.Sprintf("downtime_events_%s_%s", year, month)
		if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, old, renamed)); err != nil {
			return fmt.Errorf("v57 rename %s -> %s: %w", old, renamed, err)
		}
	}
	return nil
}

// v56SourceabilityEvents persists the verdict changes SourceabilityMonitor
// already computes and throws away every two minutes.
//
// The monitor recomputes per-(process, style) sourceability, already runs the
// edge-triggered diff (wireChanged), broadcasts the result to Edge — and
// stores nothing. The 2026-07-21 incident's root physical condition was zero
// system stock on 74577-6SA0A.06. ShinGo knew, continuously, and did not write
// it down.
//
// Column vocabulary follows bin_uop_audit (op / source / actor / metadata)
// rather than inventing a new one — that table is the newest and best-designed
// shape in this schema and new tables converge on it.
//
// Write volume is near zero in steady state: edge-triggered on an
// operator-visible verdict change, so dozens of verdicts per cycle produce
// rows only when one actually moves.
func v56SourceabilityEvents(tx *sql.Tx) error {
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS sourceability_events (
			id              BIGSERIAL PRIMARY KEY,
			process_key     TEXT NOT NULL,
			style_id        TEXT NOT NULL DEFAULT '',
			payload_code    TEXT NOT NULL DEFAULT '',
			sourceable      BOOLEAN NOT NULL,
			status          TEXT NOT NULL DEFAULT '',
			reason          TEXT NOT NULL DEFAULT '',
			missing_payload TEXT NOT NULL DEFAULT '',
			op              TEXT NOT NULL DEFAULT 'sourceability_change',
			source          TEXT NOT NULL DEFAULT '',
			actor           TEXT NOT NULL DEFAULT 'system',
			metadata        JSONB,
			observed_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		// "when did this process last change verdict" and "what happened to
		// this payload" are the two questions; both are covered.
		`CREATE INDEX IF NOT EXISTS idx_sourceability_events_key_time
			ON sourceability_events(process_key, style_id, observed_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sourceability_events_payload_time
			ON sourceability_events(missing_payload, observed_at DESC)
			WHERE missing_payload <> ''`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("v56 %q: %w", stmt, err)
		}
	}
	return nil
}

// v55OrderHistoryReasonColumns adds the typed reason to the history row.
//
// order_history is the oldest and most load-bearing table in the schema and
// has been (order_id, status, detail, created_at) with one index the whole
// time. `detail` is prose, so every consumer that wanted a category had to
// substring-match it — the bug class that once classified 100% of failures as
// "Robot blocked" by finding a word in a sentence.
//
//	code   protocol.TermCode on a terminal row, protocol.QueueCode on a
//	       →queued row. Both are typed, exhaustiveness-tested vocabularies.
//	actor  who caused it. Already set at every Fail/Skip call site and
//	       dropped on the floor by transition().
//	ref    what the reason CONCERNS — the VDA 5050 errorReferences idea.
//	       JSONB so `ref->>'payload'` is a GROUP BY rather than a LIKE.
//
// queue_code on the →queued row is the other half. orders.queue_code is a LIVE
// column overwritten in place, so it only ever answers "what is stuck right
// now" — which is the single reason starvation-by-cause has been unqueryable.
//
// Additive and nullable. No backfill: rows written before this have no code,
// and inferring one from `detail` would rebuild the substring matching this
// replaces. Uncoded is the honest value.
func v55OrderHistoryReasonColumns(tx *sql.Tx) error {
	for _, stmt := range []string{
		`ALTER TABLE order_history ADD COLUMN IF NOT EXISTS code TEXT`,
		`ALTER TABLE order_history ADD COLUMN IF NOT EXISTS actor TEXT`,
		`ALTER TABLE order_history ADD COLUMN IF NOT EXISTS ref JSONB`,
		// Purpose-built, matching bin_uop_audit's shape: the questions are
		// "what happened for this reason" and "what happened to this payload".
		// Partial — the columns are NULL on every pre-migration row and on
		// every uncoded transition, which is most of them.
		`CREATE INDEX IF NOT EXISTS idx_order_history_code
			ON order_history(code, created_at) WHERE code IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_order_history_ref_payload
			ON order_history((ref->>'payload')) WHERE ref IS NOT NULL`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("v55 %q: %w", stmt, err)
		}
	}
	return nil
}

func v54BackfillMissionEventRobotID(tx *sql.Tx) error {
	_, err := tx.Exec(`UPDATE mission_events me
		SET robot_id = o.robot_id
		FROM orders o
		WHERE me.order_id = o.id
		  AND me.robot_id = ''
		  AND o.robot_id <> ''`)
	return err
}

// v58DropLinesideCoreNodeNameDefault removes the DEFAULT ” that v21 had to
// put on lineside_buckets.core_node_name to add the column to a populated
// table, and that a fresh install has never had.
//
// Dropping a default does not touch existing rows — it only stops future
// inserts from silently filling in the UNKNOWN value for a column that is part
// of the table's uniqueness. Every insert site names the column explicitly, so
// nothing loses a value it was relying on.
//
// THIS IS THE FORWARD HALF ONLY, and the data half is deliberately not here.
// If a plant already carries lineside_buckets rows with core_node_name = ”,
// they are still there and still inside
// UNIQUE (station, core_node_name, pair_key, style_id, part_number) — where
// unrelated nodes' buckets would have collided into one row. A blind DELETE or
// backfill from this migration would be guessing at data it cannot see.
//
// So it is a question for the plant-dump rehearsal (B1/O5), one query:
//
//	SELECT count(*) FROM lineside_buckets WHERE core_node_name = '';
//
// If that is zero at every plant, say so HERE and the question is closed
// forever. If it is not, the cleanup is its own migration written against
// what the rows actually turn out to be.
func v58DropLinesideCoreNodeNameDefault(tx *sql.Tx) error {
	if !schema.ColumnExists(tx, "lineside_buckets", "core_node_name") {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE lineside_buckets ALTER COLUMN core_node_name DROP DEFAULT`); err != nil {
		return fmt.Errorf("drop lineside_buckets.core_node_name default: %w", err)
	}
	return nil
}

// v59DemandOrigins creates the demand grain: one row per continuous period
// during which a place needed material, plus the link from each order back to
// the demand it served.
//
// THIS IS THE TABLE'S ONLY HOME. It is deliberately NOT in postgres_ddl.go,
// and that is not a style preference.
//
// schema.Apply runs the baseline DDL before the versioned migrations, on both
// fresh and aged databases. So a CREATE TABLE IF NOT EXISTS in the baseline
// always wins and the migration's copy never runs anywhere — two copies of
// one DDL with only one live, and no test able to tell you they disagree,
// because both convergence paths would be sourcing the table from the
// baseline. That was verified the expensive way: a deliberate DEFAULT
// divergence between the two copies passed TestSchemaConvergesAcrossVintages.
//
// Single-homed here, both convergence paths run THIS code, so a divergence
// has somewhere to show up. sourceability_events (v56) is the precedent.
//
// The orders ALTERs below are a different case and correctly live in three
// places — see the comment on them.
func v59DemandOrigins(tx *sql.Tx) error {
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS demand_origins (
		    origin_id         UUID PRIMARY KEY,
		    episode_key       TEXT NOT NULL,
		    kind              TEXT NOT NULL,
		    direction         TEXT NOT NULL DEFAULT '',
		    trigger_kind      TEXT NOT NULL DEFAULT '',
		    trigger_ref       TEXT NOT NULL DEFAULT '',
		    parent_origin_id  UUID,
		    station_id        TEXT NOT NULL DEFAULT '',
		    process_id        BIGINT NOT NULL DEFAULT 0,
		    core_node_name    TEXT NOT NULL DEFAULT '',
		    payload_code      TEXT NOT NULL DEFAULT '',
		    opened_at         TIMESTAMPTZ NOT NULL,
		    opened_total      INTEGER NOT NULL DEFAULT 0,
		    threshold         INTEGER NOT NULL DEFAULT 0,
		    used_edge_reports BOOLEAN NOT NULL DEFAULT false,
		    revision          BIGINT NOT NULL DEFAULT 1,
		    expected_orders   INTEGER,
		    expected_reason   TEXT NOT NULL DEFAULT '',
		    uop_delivered     INTEGER NOT NULL DEFAULT 0,
		    rerequest_count   INTEGER NOT NULL DEFAULT 0,
		    signal_count      INTEGER NOT NULL DEFAULT 0,
		    discretionary     BOOLEAN NOT NULL DEFAULT false,
		    closed_at         TIMESTAMPTZ,
		    close_reason      TEXT NOT NULL DEFAULT ''
		)`); err != nil {
		return fmt.Errorf("create demand_origins: %w", err)
	}
	if _, err := tx.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_demand_origins_open_key
		    ON demand_origins(episode_key) WHERE closed_at IS NULL`); err != nil {
		return fmt.Errorf("create demand_origins open-key index: %w", err)
	}
	if _, err := tx.Exec(`
		CREATE INDEX IF NOT EXISTS idx_demand_origins_opened_at
		    ON demand_origins(opened_at)`); err != nil {
		return fmt.Errorf("create demand_origins opened_at index: %w", err)
	}
	// Idempotent, and deliberately duplicating migrateAddBaselineColumns:
	// that runs pre-baseline on every startup to keep an aged DB bootable,
	// this is the versioned record of when the columns became required. Same
	// pairing queue_code/queue_cause already use.
	if _, err := tx.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS origin_id UUID`); err != nil {
		return fmt.Errorf("add orders.origin_id: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS origin_class TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add orders.origin_class: %w", err)
	}
	// idx_orders_origin_id is NOT created here. Every other idx_orders_* lives
	// in postgres_ddl.go, which runs ahead of this and would win — the same
	// only-one-copy-runs trap as the table above, so the index keeps its home
	// with its siblings and this migration does not carry a second copy.
	return nil
}

// v60DemandOriginClosedBy records which mechanism ended an episode:
// "notification" (a close path fired) or "sweep" (the reconciler noticed).
//
// NO DEFAULT, DELIBERATELY. NULL means "the sender did not say" — an older Edge,
// or a row written before this column existed — and that is a different fact
// from "the notification path closed it". Defaulting to 'notification' would
// make those two indistinguishable and would report the notification paths as
// carrying work they may never have done. That is the lying-default pattern
// this branch has now hit six times: absence of data rendering as presence of a
// finding.
func v60DemandOriginClosedBy(tx *sql.Tx) error {
	if _, err := tx.Exec(
		`ALTER TABLE demand_origins ADD COLUMN IF NOT EXISTS closed_by TEXT`); err != nil {
		return fmt.Errorf("add demand_origins.closed_by: %w", err)
	}
	return nil
}

// v61OrphanAgedAt moves orphan aging off origin_class and onto its own
// nullable timestamp.
//
// SINGLE-HOMED HERE, NOT IN THE BASELINE. postgres_ddl.go runs ahead of the
// versioned migrations on every startup, fresh and aged alike, so a column
// declared in both places has one live copy and one dead one and no test can
// report which is which — the trap B9 was written for.
//
// orders.origin_id/origin_class are the one exception on this table and they
// earn it: the BASELINE declares idx_orders_origin_id, an index that runs
// inside schema.Apply ahead of this whole pipeline, so an aged plant DB needs
// the column present before then or startup dies on "column does not exist".
// NOTHING INDEXES orphan_aged_at, so it has no such claim and gets one home.
// Convergence still holds because an ALTER runs identically on both paths —
// what a baseline CREATE TABLE would break is the aged path, where the CREATE
// is a no-op.
//
// The UPDATE is for trees that already ran the fourth value. `orphan_aged` was
// only ever written by the reconciling sweep and never by any create site, so
// reversing it is exact rather than a guess: the class goes back to what the
// create site stamped, and the aging that value was carrying is preserved in
// the new column instead of being thrown away. NOW() rather than NULL because
// those rows HAVE aged and merely had nowhere to say so — NULL is reserved for
// "still a live finding", and defaulting them to it would resurrect every
// retired orphan as a fresh alarm.
//
// Literal 'orphan_aged'/'orphan' rather than the protocol constants: the
// constant for the fourth value is deleted in this same change, and a migration
// is a historical record of what a database HELD, not a re-render of today's
// vocabulary. A migration that renders constants stops describing the past the
// moment somebody edits one. migrations.go imports no protocol for this reason,
// even though the rest of package store does.
func v61OrphanAgedAt(tx *sql.Tx) error {
	if _, err := tx.Exec(
		`ALTER TABLE orders ADD COLUMN IF NOT EXISTS orphan_aged_at TIMESTAMPTZ`); err != nil {
		return fmt.Errorf("add orders.orphan_aged_at: %w", err)
	}
	if _, err := tx.Exec(`
		UPDATE orders
		   SET origin_class   = 'orphan',
		       orphan_aged_at = COALESCE(orphan_aged_at, NOW())
		 WHERE origin_class = 'orphan_aged'`); err != nil {
		return fmt.Errorf("convert orphan_aged orders back to orphan: %w", err)
	}
	return nil
}

// v62SceneEdgeControlHandles adds the cubic-Bezier control handles to
// scene_edges.
//
// The verify checks ctrl2_y — the LAST column this adds, for the reason v59
// records: everything here runs in one transaction so a partial apply is not
// reachable, but a post-condition that passes while the tail is missing would
// be a self-heal that never heals, and which end it checks costs nothing to
// get right.
func v62SceneEdgeControlHandles(tx *sql.Tx) error {
	for _, col := range []string{"ctrl1_x", "ctrl1_y", "ctrl2_x", "ctrl2_y"} {
		if _, err := tx.Exec(
			`ALTER TABLE scene_edges ADD COLUMN IF NOT EXISTS ` + col + ` DOUBLE PRECISION`); err != nil {
			return fmt.Errorf("add scene_edges.%s: %w", col, err)
		}
	}
	return nil
}

// columnHasDefault reports whether a column carries a column default.
//
// Used as a migration post-condition. Returns false when the table or column
// is absent, which reads correctly for the verify: "there is no default here".
func columnHasDefault(q schema.Querier, table, column string) bool {
	var has bool
	q.QueryRow(
		`SELECT COALESCE(column_default IS NOT NULL, false)
		   FROM information_schema.columns
		  WHERE table_name = $1 AND column_name = $2`,
		table, column,
	).Scan(&has)
	return has
}

// v63DemandOriginProcessIDText converts demand_origins.process_id from the Edge
// SQLite row id to the Edge process NAME, matching process_styles.process_id,
// style_claims.process_id and PlantClaimsReport.ProcessID.
//
// THE USING CLAUSE IS A CAST AND NOT A LOOKUP, and it has to be: Core has no
// table mapping an Edge row id to an Edge process name — that mapping is exactly
// what did not exist and is the reason for the change. So any row that somehow
// predates this migration keeps its number as the string of that number, which
// is a value that joins nothing. That is the honest outcome and it costs nothing
// real, because no plant has run v59: the only databases this can touch are dev
// boxes and the sim, where the table is empty or disposable.
//
// The DEFAULT moves with the type. It was 0, meaning "no process"; ” means the
// same thing for a text column and matches what every other identity column on
// this table already uses for absence.
func v63DemandOriginProcessIDText(tx *sql.Tx) error {
	// DROP THE DEFAULT FIRST. Postgres will not re-type a column whose default
	// cannot be cast to the new type, and 0 -> text is exactly that case; the
	// error it gives ("default for column ... cannot be cast automatically")
	// names the default and not the column, which is a confusing place to land.
	if _, err := tx.Exec(
		`ALTER TABLE demand_origins ALTER COLUMN process_id DROP DEFAULT`); err != nil {
		return fmt.Errorf("drop demand_origins.process_id default: %w", err)
	}
	if _, err := tx.Exec(`
		ALTER TABLE demand_origins
		    ALTER COLUMN process_id TYPE TEXT
		    USING (CASE WHEN process_id = 0 THEN '' ELSE process_id::text END)`); err != nil {
		return fmt.Errorf("retype demand_origins.process_id to text: %w", err)
	}
	if _, err := tx.Exec(
		`ALTER TABLE demand_origins ALTER COLUMN process_id SET DEFAULT ''`); err != nil {
		return fmt.Errorf("set demand_origins.process_id default: %w", err)
	}
	return nil
}

// v64EdgeRegistryHostnameBinding adds the duplicate-edge detector's columns.
//
// Metadata-only on every plant: four nullable-or-non-volatile-defaulted adds on
// a table holding one row per station (Springfield: one). PG 11+ does not
// rewrite for these, which is the same property the B1 rehearsal measured
// across migrations 53-62.
//
// THE BACKFILL BINDS EACH LIVE PLANT TO THE BOX IT IS ALREADY ON. Without it,
// bound_hostname sits at ” and the first register after deploy claims it —
// same end state, but it means the deploy itself is a window in which a
// hostname change is accepted without comment. The whole guard is that no
// rebind happens without somebody being told, so it must not start life with
// one.
//
// The predicate is `hostname <> ”`: a row created by UpdateHeartbeat's minimal
// insert has never carried a hostname, so there is nothing to bind it to and ”
// correctly means "unclaimed".
func v64EdgeRegistryHostnameBinding(tx *sql.Tx) error {
	for _, ddl := range []string{
		`ALTER TABLE edge_registry ADD COLUMN IF NOT EXISTS bound_hostname TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE edge_registry ADD COLUMN IF NOT EXISTS conflict_hostname TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE edge_registry ADD COLUMN IF NOT EXISTS conflict_count BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE edge_registry ADD COLUMN IF NOT EXISTS conflict_at TIMESTAMPTZ`,
	} {
		if _, err := tx.Exec(ddl); err != nil {
			return fmt.Errorf("edge_registry column add: %w", err)
		}
	}
	if _, err := tx.Exec(
		`UPDATE edge_registry SET bound_hostname = hostname
		  WHERE bound_hostname = '' AND hostname <> ''`); err != nil {
		return fmt.Errorf("bind existing edge_registry rows to their current hostname: %w", err)
	}
	return nil
}

// LinesideBucketsUniqueConstraint is the name of the uniqueness constraint on
// lineside_buckets, declared once and used by BOTH homes — the baseline
// CREATE TABLE in postgres_ddl.go and v65 below.
//
// NAMED, NOT AUTO-NAMED, AND THAT IS THE POINT OF THE CONSTANT. An inline
// `UNIQUE (...)` gets a name Postgres derives from the column list, so a fresh
// database and a database migrated into the same shape end up with the same
// constraint under two different names. TestSchemaConvergesAcrossVintages
// compares canonicalised pg_dump output and Canonical does not strip
// constraint names — it sorts statements — so that difference is a
// convergence failure waiting for whoever next runs the docker suite.
const LinesideBucketsUniqueConstraint = "lineside_buckets_node_pair_style_part_key"

// v65LinesideBucketsDropStationFromKey removes `station` from the
// lineside_buckets uniqueness key, leaving
// (core_node_name, pair_key, style_id, part_number).
//
// THIS IS A PREREQUISITE OF PER-EDGE IDENTITY, NOT A FOLLOW-UP TO IT, and the
// distinction is the whole reason it lands now. `station` is a CONSTANT across
// a plant today — Springfield's edge_registry holds exactly one row and all
// 4 lineside_buckets rows, all 62 demand_registry rows and all 257
// inventory_delta_dedup rows carry the single value `plant-a.line-1`. A
// one-valued column is not a discriminator, so today's key is already
// (core_node_name, pair_key, style_id, part_number) in everything but
// spelling, and this migration merges zero rows.
//
// The day each edge gets a distinct id is the day it starts discriminating,
// and then the table breaks in a direction that hides itself:
//
//   - The three WRITE statements in ApplyLinesideBucketDelta are all
//     station-scoped — the conflict target, the negative-delta existence
//     check, and the qty=0 garbage collect.
//   - The two READ paths are station-BLIND — SystemUOPForPayload's bucket
//     term (service/inventory_system_count.go) and the per-node ledger
//     (service/inventory_lineside_ledger.go) both GROUP BY without ever
//     mentioning station.
//
// Move a process between edges with the column discriminating and the same
// physical bucket exists as two rows: the new edge's writes never GC the old
// edge's row, and the station-blind SUM counts both. An inflated on-hand total
// suppresses replenishment — the Springfield 74576 failure reached by another
// route, and one nobody would look for in a uniqueness constraint.
//
// THE REPO HAS ALREADY MADE AND FIXED THIS EXACT MISTAKE IN THIS EXACT KEY.
// v21 (Round-3 Obs 8) replaced `node_id BIGINT` because Edge's int64 namespace
// was being used as if it were Core's, producing the Springfield 6883
// stuck-bucket and the Hopkinsville Core-only orphan. `station` is the
// surviving half of that same key and the same mistake: an Edge-scoped
// identifier inside the identity of a Core-owned physical fact. Which node
// holds the parts is the fact; which Pi mentioned it is not.
//
// The column SURVIVES as attribute data — last reporter, useful forensics,
// same status `edge_registry.hostname` has. What it stops being is identity.
// It is deliberately NOT rewritten to some plant-level constant: Core has no
// plant identifier to write, and inventing one here would land a second
// identity concept ahead of the first.
//
// REFUSES RATHER THAN MERGES. If a database somehow does hold two rows
// differing only by station, they are two claims about one physical bucket and
// summing them would be the double-count this migration exists to prevent —
// while keeping one would silently discard counted parts. Springfield is
// measured clean (4 rows, 4 distinct keys). A plant that is not is a finding,
// and the deploy should stop and produce one.
func v65LinesideBucketsDropStationFromKey(tx *sql.Tx) error {
	var dupes int
	if err := tx.QueryRow(`
		SELECT COALESCE(SUM(n - 1), 0) FROM (
			SELECT count(*) AS n
			FROM lineside_buckets
			GROUP BY core_node_name, pair_key, style_id, part_number
			HAVING count(*) > 1
		) d`).Scan(&dupes); err != nil {
		return fmt.Errorf("check lineside_buckets for cross-station duplicates: %w", err)
	}
	if dupes > 0 {
		return fmt.Errorf(
			"lineside_buckets holds %d row(s) that differ only by `station` — dropping station from the "+
				"uniqueness key would merge them, and neither merge is safe: summing double-counts one "+
				"physical bucket, keeping one discards counted parts. This also contradicts the premise "+
				"of the change, which is that `station` is a single plant-wide value. Resolve the rows "+
				"first:\n"+
				"  SELECT station, core_node_name, pair_key, style_id, part_number, qty FROM lineside_buckets\n"+
				"   WHERE (core_node_name, pair_key, style_id, part_number) IN (\n"+
				"     SELECT core_node_name, pair_key, style_id, part_number FROM lineside_buckets\n"+
				"      GROUP BY 1,2,3,4 HAVING count(*) > 1)\n"+
				"   ORDER BY core_node_name, part_number, station", dupes)
	}

	// Drop whichever unique constraint currently carries `station`, by
	// introspection: it may be v21's explicit name or the baseline's
	// auto-generated one, and which of the two a given database has depends on
	// the vintage it was created at. Same pg_constraint pattern v21 used, one
	// step narrower — only constraints whose definition mentions the column.
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
				  AND pg_get_constraintdef(con.oid) LIKE '%station%'
			LOOP
				EXECUTE 'ALTER TABLE lineside_buckets DROP CONSTRAINT ' || quote_ident(c.conname);
			END LOOP;
		END $$`); err != nil {
		return fmt.Errorf("drop station-bearing lineside_buckets unique constraint: %w", err)
	}

	// Add the station-free constraint under its declared name. Guarded because
	// Postgres has no ADD CONSTRAINT IF NOT EXISTS, and because a fresh
	// database got this constraint from the baseline before the migration
	// pipeline ran.
	if _, err := tx.Exec(fmt.Sprintf(`DO $$
		BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint con
				JOIN pg_class rel ON rel.oid = con.conrelid
				WHERE rel.relname = 'lineside_buckets' AND con.contype = 'u'
			) THEN
				ALTER TABLE lineside_buckets
					ADD CONSTRAINT %s UNIQUE (core_node_name, pair_key, style_id, part_number);
			END IF;
		END $$`, LinesideBucketsUniqueConstraint)); err != nil {
		return fmt.Errorf("add station-free lineside_buckets unique constraint: %w", err)
	}
	return nil
}

// v66EdgeIdentity splits the one string that was doing three jobs.
//
// THE COLUMN ADDS ARE METADATA-ONLY. Six adds, all either nullable or
// non-volatile-defaulted, on a table holding one row per station (Springfield:
// one, measured). PG 11+ does not rewrite for these — the same property the B1
// rehearsal measured across 53-62 and v64 relied on.
//
// THE BACKFILL IS THE WHOLE COMPATIBILITY WINDOW, and it is one line:
//
//	station_uid = station_id
//
// After it runs, the legacy Edge at both plants — which sends
// `station_id: plant-a.line-1` and knows nothing about uids — registers by a
// uid that IS 'plant-a.line-1', so `WHERE station_uid = $1` finds the row it
// has always found. "Core accepts either" is not a branch anywhere in the
// code; it is this UPDATE making the two spellings the same string for one
// deploy. That is why guards 1-3 can ship in their final form NOW and the
// separate deploy only has to delete a recovery branch — there is no dual
// acceptance path to unwind.
//
// The uid stops being the legacy string at enrollment, which is a deliberate
// human act (Core mints, an operator puts it in shingoedge.yaml). Until then
// the plant runs on a uid that reads like its old name, which is exactly the
// behaviour a rollback wants.
//
// display_name = station_id for the same reason: the operator-facing string
// starts as what the operator already sees, and is free to change from the
// first minute WITHOUT touching anything the wire or the history is keyed on.
// That is the property the rename case never had.
//
// bound_at = registered_at where a hostname binding already exists, so v64's
// leases carry a plausible age instead of NULL. bound_instance stays ” — no
// Edge has ever sent one, and inventing a value would make the first register
// after deploy look like a lease MOVE rather than the first claim it is.
//
// line_ids IS DROPPED, and dropping it is the retirement rather than half of
// one. Leaving the column would leave Springfield's stored ["line-1"] in place
// and the phantom dashboard scope 'plant-a.line-1.line-1' with it; the field
// shipped []string{cfg.LineID} regardless of any station override, so it never
// carried information, only a wrong composition. Its sole consumer
// (apiStations) is deleted in the same commit.
func v66EdgeIdentity(tx *sql.Tx) error {
	for _, ddl := range []string{
		`ALTER TABLE edge_registry ADD COLUMN IF NOT EXISTS station_uid TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE edge_registry ADD COLUMN IF NOT EXISTS display_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE edge_registry ADD COLUMN IF NOT EXISTS bound_instance TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE edge_registry ADD COLUMN IF NOT EXISTS prev_instance TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE edge_registry ADD COLUMN IF NOT EXISTS bound_at TIMESTAMPTZ`,
	} {
		if _, err := tx.Exec(ddl); err != nil {
			return fmt.Errorf("v66 edge_registry column add: %w", err)
		}
	}
	if _, err := tx.Exec(
		`UPDATE edge_registry
		    SET station_uid  = CASE WHEN station_uid  = '' THEN station_id ELSE station_uid END,
		        display_name = CASE WHEN display_name = '' THEN station_id ELSE display_name END,
		        bound_at     = CASE WHEN bound_at IS NULL AND bound_hostname <> ''
		                            THEN registered_at ELSE bound_at END`); err != nil {
		return fmt.Errorf("v66 backfill edge identity from the legacy station id: %w", err)
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS edge_registry_station_uid_key
		ON edge_registry (station_uid) WHERE station_uid <> ''`); err != nil {
		return fmt.Errorf("v66 station_uid unique index: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE edge_registry DROP COLUMN IF EXISTS line_ids`); err != nil {
		return fmt.Errorf("v66 drop retired line_ids: %w", err)
	}
	return nil
}

// v67EdgeClaim adds the acknowledgement mark, and it is the schema half of
// letting an edge introduce itself.
//
// # WHY THIS EXISTS, AND WHY IT IS NOT A REVERSION
//
// v66 made Core the only minter of station identity, and the enrollment deploy
// deleted the branch that let an unknown edge register. Both were right about
// the defect they were closing: a station id DERIVED FROM TWO CONFIG DEFAULTS
// was the same string on every unconfigured Pi in the fleet, so a second box
// did not collide with the first — it took turns owning the first one's row,
// and the clause that let it do so erased the evidence there had been two.
//
// But closing that shut something else that was never the problem: an edge
// being able to come up AT ALL without a human first fetching a value from
// another system. That property is what made an edge deployable by whoever was
// holding the SD card, and losing it moves a distributed-identity concept onto
// the shop floor, where it does not belong.
//
// THE DISTINCTION THE ORIGINAL FIX MISSED IS BETWEEN COLLISION AND CREATION.
// The danger was never that a machine could create a row. It was that it could
// create THE SAME row as another machine, because the string was derived rather
// than drawn. An edge that mints 64 bits of randomness cannot take over another
// station's row no matter how many are deployed — it can only ever make its
// own. So the machine may introduce itself; what it may not do is assert WHICH
// station it is. That remains a human act, and this column is where the human's
// answer is recorded.
//
// NULL = introduced, running, unacknowledged. A timestamp = somebody looked at
// it and said what it is.
//
// THE BACKFILL SETS EVERY EXISTING ROW CLAIMED, and that is not a convenience.
// Every row that exists when this runs got there one of two ways: an operator
// called Enroll, or v66 backfilled it from the station id a plant has been
// running on for months. Both are acknowledged by definition. Leaving them NULL
// would open the deploy by announcing that both live plants are unidentified,
// which is false and is exactly the kind of alarm that teaches people to ignore
// the column.
func v67EdgeClaim(tx *sql.Tx) error {
	if _, err := tx.Exec(
		`ALTER TABLE edge_registry ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ`); err != nil {
		return fmt.Errorf("v67 add claimed_at: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE edge_registry SET claimed_at = registered_at WHERE claimed_at IS NULL`); err != nil {
		return fmt.Errorf("v67 backfill claimed_at from registered_at: %w", err)
	}
	return nil
}

// v68SupplyRefusals adds Core's record of supply refusals.
//
// WHY CORE HOLDS THIS AT ALL. Every other fact on this table's subject matter is
// something Core can compute: how many bins exist, what a style needs, whether a
// pool is dry. This one it cannot. Shingo's coverage is a SUBSET of the greater
// Martinrea system — material in receiving, on an unscanned rack, in an untracked
// tote is invisible in both directions — so "there are none" is a statement only
// a person who walked the floor can make. Core stores it because it is the one
// inventory fact in the system with a human author, and because it is the only
// bridge across that gap that could ever close it.
//
// Two consumers beyond the round trip, both owner-stated: cross-edge supply, and
// Core-side operators seeing what happened.
//
// NOT open-state-only, unlike the edge's mirror. The edge deletes on resolution
// because it renders live cards; Core keeps history, which is the same division
// demand_origins already draws — "the history lives on Core, which is the service
// that keeps history."
func v68SupplyRefusals(tx *sql.Tx) error {
	_, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS supply_refusals (
		    id              BIGSERIAL PRIMARY KEY,
		    loader_node     TEXT NOT NULL,
		    payload_code    TEXT NOT NULL,
		    station_id      TEXT NOT NULL DEFAULT '',
		    refused_at      TIMESTAMPTZ NOT NULL,
		    refused_by      TEXT NOT NULL DEFAULT '',
		    ack_at          TIMESTAMPTZ,
		    ack_choice      TEXT NOT NULL DEFAULT '',
		    ack_process_id  TEXT NOT NULL DEFAULT '',
		    closed_at       TIMESTAMPTZ,
		    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		-- One OPEN refusal per card. Partial, so history accumulates behind it:
		-- a card refused, resolved and refused again is two rows, which is the
		-- point of Core keeping history at all.
		CREATE UNIQUE INDEX IF NOT EXISTS idx_supply_refusals_open
		    ON supply_refusals (loader_node, payload_code) WHERE closed_at IS NULL;
		CREATE INDEX IF NOT EXISTS idx_supply_refusals_payload
		    ON supply_refusals (payload_code, refused_at DESC);
	`)
	return err
}

// v71OrdersUUIDUnique replaces the plain edge_uuid index with the partial unique
// one the plants already run.
//
// Two orders sharing an edge_uuid is not a shape this system has a story for:
// GetByUUID resolves the ambiguity with `ORDER BY id DESC LIMIT 1`, so a
// duplicate silently makes every lookup pick the newer row — including the
// ownership check behind cancel and release, which would then act on an order
// nobody named.
//
// Empty is excluded rather than cleaned up. A blank edge_uuid means "no Edge
// asked for this"; several rows can honestly be blank at once, and rewriting
// history to satisfy an index would be inventing identifiers for orders that
// never had one.
func v71OrdersUUIDUnique(tx *sql.Tx) error {
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_orders_uuid`); err != nil {
		return fmt.Errorf("drop plain idx_orders_uuid: %w", err)
	}
	if _, err := tx.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_uuid ON orders(edge_uuid) WHERE edge_uuid <> ''`); err != nil {
		return fmt.Errorf("create unique idx_orders_uuid: %w", err)
	}
	return nil
}

// v72StationCoreOperator renames the operator screen's station id everywhere a
// database stores it.
//
// The screen is the operator's, not a "spot" anything — "spot order" was never
// a word anyone at a plant used. The rename is cheap on the code side, because
// nothing reads the literal: the class each order belongs to is stamped at the
// create site, so no downstream branch depends on the spelling.
//
// It is NOT cheap on the data side, which is why this migration exists rather
// than a forward-only change of the two write sites. Every one of these tables
// is grouped or filtered by the exact string:
//
//   - orders.station_id — the orphan-by-site summary GROUPs BY it, so a
//     forward-only rename splits one station into two rows forever, and every
//     count, and the oldest-live timestamp with them.
//   - mission_telemetry.station_id — copied from the order's station, so it
//     splits exactly the same way.
//   - dashboards.stations_json — a saved board naming the old string matches
//     nothing once new orders carry the new one. The board does not break; it
//     goes BLANK, which is this system's signature failure and the reason the
//     whole campaign exists.
//   - cell_config.station — cross-filtered against the dashboard scope above,
//     so the two have to move together or a heartbeat board empties.
//
// The station picker needs no migration: it is SELECT DISTINCT over
// orders.station_id, so it offers the new name the moment the first statement
// below commits.
//
// Idempotent by construction: every statement is conditional on finding the OLD
// value, so a re-run matches nothing. That matters more than usual here — a
// failing verify re-runs the migration on every boot, and a data migration that
// is not safe to repeat would compound each time.
//
// stations_json is TEXT holding a JSON array of bare strings, so the rename is
// a replace of the QUOTED token. The quotes are what make it exact: they pin
// the match to a whole element, and no other station id contains this one.
//
// Historical edge_uuid strings keep the old spelling. They are identifiers that
// were minted, not fields that describe anything, and rewriting them would be
// inventing a past.
func v72StationCoreOperator(tx *sql.Tx) error {
	for _, s := range []struct{ what, stmt string }{
		{"orders.station_id",
			`UPDATE orders SET station_id = 'core-operator' WHERE station_id = 'core-spot'`},
		{"mission_telemetry.station_id",
			`UPDATE mission_telemetry SET station_id = 'core-operator' WHERE station_id = 'core-spot'`},
		{"cell_config.station",
			`UPDATE cell_config SET station = 'core-operator' WHERE station = 'core-spot'`},
		{"dashboards.stations_json",
			`UPDATE dashboards SET stations_json = replace(stations_json, '"core-spot"', '"core-operator"')
			  WHERE stations_json LIKE '%"core-spot"%'`},
	} {
		if _, err := tx.Exec(s.stmt); err != nil {
			return fmt.Errorf("v72 rename %s: %w", s.what, err)
		}
	}
	return nil
}

// noCoreSpotLeft reports whether the old station id is gone from every table
// that stores one.
//
// EXISTS rather than counts, and one round trip rather than four: this runs on
// every Core startup for every applied migration, and the answer is the same
// either way.
func noCoreSpotLeft(q schema.Querier) bool {
	var clean bool
	if err := q.QueryRow(`
		SELECT NOT (
		       EXISTS (SELECT 1 FROM orders            WHERE station_id = 'core-spot')
		    OR EXISTS (SELECT 1 FROM mission_telemetry WHERE station_id = 'core-spot')
		    OR EXISTS (SELECT 1 FROM cell_config       WHERE station    = 'core-spot')
		    OR EXISTS (SELECT 1 FROM dashboards        WHERE stations_json LIKE '%"core-spot"%')
		)`).Scan(&clean); err != nil {
		return false
	}
	return clean
}

// v73OrdersUUIDUniqueExemptRestore narrows the unique index to skip the one
// remaining derived edge_uuid.
//
// v71 made the column unique on the sound grounds that two orders sharing an
// edge_uuid is a shape with no story: GetByUUID resolves the ambiguity with
// ORDER BY id DESC, so the ownership check behind cancel and release acts on an
// order nobody named. What v71 could not see is that two edge_uuid values in
// this system are not identities at all — they are structural names built from
// other rows, and neither is unique. Compound children were one; they now mint a
// real UUID (dispatch/compound.go) and need no exemption.
//
// The other is the synthetic restore parent: "restore-<complexParentID>-<binID>"
// (dispatch/restore_listeners.go). It cannot be minted, because it is not
// decoration there — it is the ONLY durable link back to the complex parent.
// That parent sets no parent_order_id, and the in-memory map holding the link
// does not survive a restart, so the string is parsed back (fmt.Sscanf,
// "restore-%d-") to rebuild it. Minting a UUID would delete the link.
//
// EXPIRY CONDITION, and it is a real one rather than a hope: the `refactor-phase1`
// branch deletes the entire put-back subsystem — restore_listeners.go, the
// pending_restocks table, and this format with them — replacing the crash
// recovery with durable lane-hold reservation rows. When that lands, drop this
// exemption and restore the plain not-blank predicate:
//
//	WHERE edge_uuid <> ''
//
// Until then a re-restore of the same parent and bin legitimately repeats the
// name, so the index must not refuse it.
//
// Deliberately a LIKE on one literal prefix rather than a general escape hatch:
// the narrower it is, the louder it is about being temporary.
func v73OrdersUUIDUniqueExemptRestore(tx *sql.Tx) error {
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_orders_uuid`); err != nil {
		return fmt.Errorf("drop idx_orders_uuid: %w", err)
	}
	if _, err := tx.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_uuid ON orders(edge_uuid)
		     WHERE edge_uuid <> '' AND edge_uuid NOT LIKE 'restore-%'`); err != nil {
		return fmt.Errorf("create restore-exempt idx_orders_uuid: %w", err)
	}
	return nil
}

// v74LoaderFunnelWindows moves the multi-window setting onto the loader.
//
// It lived on the Edge as one plant-wide config key, which could only ever
// answer the question for every loader at once. Two shared-window loaders in the
// same plant have no reason to agree: spreading empties across windows is a
// property of how a particular loader is fed, not of the site.
//
// The column states the RESTRICTION (funnel to one window) rather than the
// capability, so its DEFAULT FALSE means "spread" — which is what the unset Edge
// key already resolved to. Applying this therefore changes nothing anywhere, and
// the same false-is-the-default-and-the-default-is-safe property holds for the
// Go zero value, the wire, and the Edge cache.
//
// Nullable was rejected: a NULL here would mean "ask the config", which is the
// two-sources-of-truth shape this migration exists to end.
func v74LoaderFunnelWindows(tx *sql.Tx) error {
	if _, err := tx.Exec(
		`ALTER TABLE bin_loaders ADD COLUMN IF NOT EXISTS funnel_windows BOOLEAN NOT NULL DEFAULT FALSE`); err != nil {
		return fmt.Errorf("v74 funnel_windows: %w", err)
	}
	return nil
}

// v75LoaderCarrierMix gives a loader a declared carrier mix.
//
// Until now a loader said nothing about carrier types at all. What it would
// accept fell out of the payload it was loading — payload_bin_types maps a part
// to its allowed types, and a part with no rules matches anything. That works
// while a loader runs one carrier type and silently does the wrong thing when it
// does not: the opportunistic empty is staged payload-BLANK on purpose, so the
// operator can pick the part at load time, and a blank payload makes the type
// rule match everything. A five-window loader meant to hold three of one type
// and two of another had nowhere to say so.
//
// TWO TABLES, BECAUSE THERE ARE TWO QUESTIONS AND THEY HAVE DIFFERENT ANSWERS.
//
// bin_loader_quotas is INTENT, and it belongs to the loader: "I want three
// 45x48, one 32x32 and one tote on hand." It is a preference, NOT a cap — the
// never-2N budget (carriers in flight plus resident must not exceed the window
// count) is untouched and stays the only limit on how many carriers exist. The
// quota decides WHICH type to fetch next inside that limit. That distinction is
// the reason this is safe to add: as a cap it would move the counting into the
// seam that the 2026-07-31 over-ordering incident was about; as a preference the
// seam counts exactly as it does today.
//
// A quota total may be LESS than the window count, and that is a legitimate
// thing to say — "four carriers at a five-window loader" — which had no
// expression before. A total greater than the window count is capped by the
// windows and never over-fetches.
//
// bin_loader_home_bin_types is PHYSICAL, and it belongs to the window: "this
// slot fits a 45x48 or a tote." A SET, not one type, because a slot can take
// more than one. Empty means the slot takes anything, which is what every slot
// does today.
//
// The two compose at fetch time: what is the loader short of, filtered by what
// this window can physically take. If the quota wants a type the free window
// cannot hold, the next shortfall that window CAN hold is used instead.
//
// Both tables start empty and empty is today's behaviour, so applying this
// changes nothing at any plant until somebody configures a loader.
//
// bin_type_id carries a real foreign key, unlike bin_uop_audit's deliberately
// unreferenced loader_id: a bin type is catalog data an operator picks from a
// list, not an event stamp that has to outlive an archive.
func v75LoaderCarrierMix(tx *sql.Tx) error {
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS bin_loader_quotas (
			loader_id   BIGINT  NOT NULL REFERENCES bin_loaders(id) ON DELETE CASCADE,
			bin_type_id BIGINT  NOT NULL REFERENCES bin_types(id)   ON DELETE CASCADE,
			want        INTEGER NOT NULL DEFAULT 0 CHECK (want >= 0),
			PRIMARY KEY (loader_id, bin_type_id)
		)`); err != nil {
		return fmt.Errorf("v75 bin_loader_quotas: %w", err)
	}
	// KEYED ON THE POSITION NODE ALONE, no loader_id. bin_loader_homes carries
	// UNIQUE(position_node_id) — one loader per member node — so the node
	// identifies the window by itself and carrying the loader too would be a
	// second copy of a fact that can disagree. It also would not work: a
	// composite foreign key needs a matching unique constraint on the referenced
	// table, and the pair does not have one. The first draft had both and the
	// migration was refused, which is the constraint doing its job.
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS bin_loader_home_bin_types (
			position_node_id BIGINT NOT NULL REFERENCES bin_loader_homes(position_node_id) ON DELETE CASCADE,
			bin_type_id      BIGINT NOT NULL REFERENCES bin_types(id) ON DELETE CASCADE,
			PRIMARY KEY (position_node_id, bin_type_id)
		)`); err != nil {
		return fmt.Errorf("v75 bin_loader_home_bin_types: %w", err)
	}
	return nil
}

// uuidIndexExemptsRestore reports whether idx_orders_uuid already carries the
// restore exemption. Like uuidIndexIsUnique, this reads the definition rather
// than the index's existence — the index exists either way and what changed is
// its predicate.
func uuidIndexExemptsRestore(q schema.Querier) bool {
	var def string
	if err := q.QueryRow(
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_orders_uuid'`).Scan(&def); err != nil {
		return false
	}
	return strings.Contains(def, "UNIQUE") && strings.Contains(def, "restore-")
}

// uuidIndexIsUnique reports whether idx_orders_uuid is already the unique form.
// Plain ColumnExists/IndexExists cannot answer this — the index exists either
// way, and what changed is its definition — so the check reads the definition.
func uuidIndexIsUnique(q schema.Querier) bool {
	var def string
	if err := q.QueryRow(
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_orders_uuid'`).Scan(&def); err != nil {
		return false
	}
	return strings.Contains(def, "UNIQUE")
}

// v77RobotConfidence creates the localization-confidence collection tables.
//
// SEER publishes rbk_report.confidence (0.0–1.0) on every /robotsStatus poll
// and Core has been discarding it at unmarshal. Nothing retains a history of
// it anywhere — no %confid% column exists in the RDS MariaDB and the RDS HTTP
// API has no history endpoint — so there is nothing to backfill and every day
// uncollected is lost permanently. That is why the shape has to be right now:
// the screens that read these tables can be rebuilt from correct data at any
// time, but the data cannot be reconstructed from correct screens.
//
// WHY EVERY ROW CARRIES ITS POSITION. Measured at Hopkinsville 2026-08-05:
// AMR-01/02/04/07 parked together read 0.95–0.97 while AMR-03/05/08 parked
// together read 0.67–0.79. Four robots look healthy and three look sick, and
// it is entirely the two parking areas. Any per-robot figure that does not
// hold location fixed is measuring where the robot spent its shift. A schema
// of (robot, time, confidence) with position "added later" would produce a
// week of data that cannot answer the question the table exists for.
//
// WHY x/y AND NOT A SEGMENT ID. Snapping a sample to a scene edge is a
// READ-time concern, deliberately. Nobody gets spatial binning right on the
// first attempt, and keeping raw coordinates means the binning strategy (grid
// vs. edge-snap vs. per-station) can be rewritten later against data already
// collected. A scene_edge_id column would freeze v1 snap logic into history.
//
// RELOC_STATUS IS STORED, AND ONLY reloc_status = 1 FEEDS STATISTICS.
// This is the load-bearing rule of the whole design and it is why the column
// exists rather than the state being filtered away at write time.
//
//	0 = FAILED    stored, EXCLUDED from statistics
//	1 = SUCCESS   stored, the ONLY state that feeds fleet median,
//	              residual, segment mean/p05
//	2 = RELOCING  never stored — the pose estimate is in flight and the
//	              confidence figure is transient garbage
//	3 = COMPLETED stored, EXCLUDED from statistics (pose is settled but
//	              the operator has not confirmed it)
//
// A robot sitting in FAILED at a charge point would otherwise drag that
// location's baseline down for every healthy robot that passes through,
// corrupting the residual for everyone else — the exact confound the residual
// exists to remove. Keeping the rows and excluding them by flag means the
// samples survive for forensics and the decision stays reversible; filtering
// them at write time would be irreversible and would blind the dataset to the
// failure it was built to catch.
//
// The complement matters just as much: a FAILED sample's confidence NUMBER
// cannot be trusted, but the FACT of the failure can be, completely. That is
// what segment_confidence_daily.reloc_failed_samples counts — "this segment
// produced 14 localization failures this week" is a stronger finding than any
// percentile and requires no faith in the value at all.
//
// reloc_status carries NO DEFAULT, deliberately, while on_task and blocked
// keep DEFAULT FALSE. For a boolean, false is the quiet default. There is no
// quiet value for this enum: defaulting it to 1 would mean any row written
// without the field set silently claims a healthy pose, which is
// "never coalesce absence into zero" wearing a different costume — and the
// optimistic direction is the dangerous one. There is exactly one writer and
// it always has the value, so an unset write should fail rather than lie.
//
// Numbered 77 deliberately. 76 is taken by the lane-occupancy migration on
// the reshuffling-work branch, which is unmerged at the time of writing, so
// this cannot collide with it whichever lands first. Same reasoning as the
// numbering note above v71.
func v77RobotConfidence(tx *sql.Tx) error {
	// robot_confidence_samples and robot_confidence_low share a column list
	// exactly. The low table is a DOUBLE-WRITE at sample time, not a
	// copy-before-drop: it is simpler, and it cannot be missed by a failed
	// job the way a nightly copy could. Raw expires in days; the low-
	// confidence trail is the forensic record and outlives it.
	const sampleColumns = `
		id           BIGSERIAL,
		vehicle_id   TEXT             NOT NULL,
		sampled_at   TIMESTAMPTZ      NOT NULL,
		confidence   DOUBLE PRECISION NOT NULL,
		x            DOUBLE PRECISION NOT NULL,
		y            DOUBLE PRECISION NOT NULL,
		angle        DOUBLE PRECISION NOT NULL,
		station      TEXT             NOT NULL DEFAULT '',
		last_station TEXT             NOT NULL DEFAULT '',
		order_id     BIGINT           NOT NULL DEFAULT 0,
		on_task      BOOLEAN          NOT NULL DEFAULT FALSE,
		blocked      BOOLEAN          NOT NULL DEFAULT FALSE,
		reloc_status SMALLINT         NOT NULL`

	stmts := []string{
		// Daily partitions, dropped by partition. A partition DROP is instant
		// and generates no vacuum work; a DELETE of a week of rows does
		// neither. No PRIMARY KEY: Postgres requires every partition-key
		// column in a unique constraint, and (id, sampled_at) would not
		// constrain anything this table needs constrained.
		`CREATE TABLE IF NOT EXISTS robot_confidence_samples (` + sampleColumns + `
		) PARTITION BY RANGE (sampled_at)`,
		`CREATE INDEX IF NOT EXISTS idx_robot_confidence_samples_vehicle_time
			ON robot_confidence_samples (vehicle_id, sampled_at)`,
		`CREATE INDEX IF NOT EXISTS idx_robot_confidence_samples_time
			ON robot_confidence_samples (sampled_at)`,

		`CREATE TABLE IF NOT EXISTS robot_confidence_low (` + sampleColumns + `
		) PARTITION BY RANGE (sampled_at)`,
		`CREATE INDEX IF NOT EXISTS idx_robot_confidence_low_vehicle_time
			ON robot_confidence_low (vehicle_id, sampled_at)`,
		`CREATE INDEX IF NOT EXISTS idx_robot_confidence_low_time
			ON robot_confidence_low (sampled_at)`,

		// The roll-ups are NOT partitioned and NOT expired. 12 robots × 365 is
		// ~4,400 rows/year and ~400 segments × 365 is ~146,000 rows/year at
		// roughly 18 MB. They are the only thing that can answer "worse than
		// last quarter" once the raw rows behind them are gone.
		//
		// residual is NULLABLE and that is load-bearing: NULL means "below
		// minimum coverage — not enough peer-comparable cells to say", which
		// is the opposite of 0.0 meaning "measured, and exactly average". The
		// distinction must survive all the way to the renderer.
		`CREATE TABLE IF NOT EXISTS robot_confidence_daily (
			day        DATE NOT NULL,
			vehicle_id TEXT NOT NULL,
			residual   DOUBLE PRECISION,
			cells      INTEGER NOT NULL,
			samples    INTEGER NOT NULL,
			mean       DOUBLE PRECISION,
			p05        DOUBLE PRECISION,
			PRIMARY KEY (day, vehicle_id)
		)`,

		// The two failure counts are COUNTS OF EVENTS, not statistics over
		// confidence values, and are the only figures here that need no trust
		// in the number the robot reported.
		//
		// BOTH counts, because one is ambiguous on its own. Fourteen failures
		// by one robot is a robot problem; fourteen failures by six robots is
		// a place problem. That is the same lesson as the residual — a bare
		// aggregate that does not hold its confound fixed ranks the wrong
		// thing — appearing for the third time in this design.
		//
		// mean/p05/min_conf are NULLABLE and that is the point of this table
		// being able to describe its own worst case. A segment whose every
		// sample that day was a localization failure has NO valid reading to
		// average, but it is the most important segment on the floor. Were
		// these NOT NULL the row could not be written at all, and the segment
		// would render as ABSENT — which every reader parses as fine. That is
		// "no data, zero and not applicable must look different"
		// (docs/ui-style-guide.md) failing silently and in the reassuring
		// direction, on the one segment where being wrong costs the most.
		// robot_confidence_daily.residual is nullable for exactly this
		// reason; these three are the same rule applied consistently.
		//
		// NOTE FOR THE ROLL-UP: an aggregate grouped over VALID samples can
		// never emit a row for a segment that has no valid samples. The job
		// must union the valid-sample aggregate with the failed-sample
		// aggregate on (day, area_name, edge_instance), or this case is
		// silently dropped while every other test still passes.
		//
		// edge_instance is resolved by snapping x/y to scene_edges AS THEY
		// WERE ON THAT DAY. A scene re-sync that renames or re-lays segments
		// starts a new series; old rows keep the old identity and are not
		// retro-mapped. That is the right trade — the alternative is
		// rewriting history — but it means a segment's series can end without
		// the segment physically changing.
		`CREATE TABLE IF NOT EXISTS segment_confidence_daily (
			day                  DATE NOT NULL,
			area_name            TEXT NOT NULL,
			edge_instance        TEXT NOT NULL,
			mean                 DOUBLE PRECISION,
			p05                  DOUBLE PRECISION,
			min_conf             DOUBLE PRECISION,
			samples              INTEGER NOT NULL,
			robots               INTEGER NOT NULL,
			reloc_failed_samples INTEGER NOT NULL DEFAULT 0,
			reloc_failed_robots  INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (day, area_name, edge_instance)
		)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v77 robot_confidence: %w", err)
		}
	}
	return nil
}

// v78ConfidenceAreaIDs adds the robot's advanced-area membership to both
// sample tables.
//
// WHY THIS IS URGENT RATHER THAN NICE. SEER publishes confidence as literal
// -0.0 while a robot is inside certain map areas — measured at Springfield
// 2026-08-06, 7.4% of all samples, every one of them inside area "8", with
// the reading recovering to ~0.83 on the first tick outside at unchanged
// speed and unchanged reloc_status. Without this column the raw table cannot
// tell "the plant does not localize here" apart from "this robot is losing
// its position", which are opposite findings with opposite owners. RDS keeps
// no history of area membership and its /scene returns advancedAreaList: [],
// so a tick that lands without it is not recoverable later. Same shape as
// confidence itself: cheap now, impossible afterwards.
//
// NULLABLE, AND NULL MEANS "NOT COLLECTED". Rows written before this
// migration genuinely do not know which area they were taken in, and '{}'
// would claim they were taken outside every area — a measurement that was
// never made, in the reassuring direction. New writes always set the column,
// so NULL marks exactly the pre-migration window and nothing else. This is
// the same NULL-vs-zero rule as robot_confidence_daily.residual, applied to
// a different type.
//
// TEXT[] rather than a delimited string: the value is a list, ids are opaque
// vendor strings, and any separator we picked would be a convention someone
// has to remember and something a future id could contain. Postgres has the
// type; use it.
func v78ConfidenceAreaIDs(tx *sql.Tx) error {
	// ALTER on the partitioned parent cascades to every existing partition,
	// so this is one statement per table regardless of how many days are
	// already on disk.
	for _, t := range []string{"robot_confidence_samples", "robot_confidence_low"} {
		if _, err := tx.Exec(`ALTER TABLE ` + t + ` ADD COLUMN IF NOT EXISTS area_ids TEXT[]`); err != nil {
			return fmt.Errorf("v78 confidence area_ids (%s): %w", t, err)
		}
	}
	return nil
}

// v79ConfidenceMapAndAlarms adds the map hash and the active alarm codes to
// both sample tables.
//
// MAP_MD5 — WHY A PLACE STATISTIC NEEDS TO KNOW WHICH MAP IT WAS TAKEN ON.
// Every per-lane figure in this system is computed by snapping a sample's
// x/y to scene_edges. That is only meaningful if the robot was localizing
// against the same map scene_edges describes, and a fleet is not guaranteed
// to be on one map. Measured at Hopkinsville 2026-08-06: eleven robots on
// Hop_20 and AMR-11 on Hop_21, connected, with RDS reporting
// current_map_invalid and holding it undispatchable. Its samples were being
// stored and would have been snapped against the majority map's geometry —
// a real reading of a real place, attributed to the wrong floor.
//
// The column is what lets the roll-up quarantine those rows instead of
// averaging them in. It cannot be reconstructed afterwards: nothing else
// anywhere records which map a given tick was taken under, and RDS keeps no
// history of it.
//
// ALARM_CODES — THE JOIN THAT DID NOT EXIST. The codes are not new to the
// process; rds.RbkReport.Alarms has been decoded all along and
// fleet.RobotStatus.Alarms populated from it. What was missing was a durable
// row carrying them NEXT TO a reading. Today the only retained copy lives in
// mission_telemetry.robot_alarms_json, attached to mission endpoints, so a
// no-estimate sample cannot be joined to the alarm that accompanied it —
// which is how "reflectors in map not enough" (54018, standing on every
// Springfield robot since the week of 2026-06-08 and naming the exact nine
// zones it means) took two days of archaeology to connect to the readings it
// explains.
//
// NULLABLE, AND NULL MEANS "NOT COLLECTED" — the same rule as v78's
// area_ids and robot_confidence_daily.residual. Rows written before this
// migration genuinely do not know their map or their alarms; '{}' would
// claim the robot was on no map and raising no alarms, both of which are
// measurements nobody made. The writer always sets both, so NULL marks
// exactly the pre-migration window and nothing else.
//
// INTEGER[] rather than a delimited string, for the reason v78 gives about
// TEXT[]: the value is a list, and any separator becomes a convention
// somebody has to remember. Whether a Go slice binds to a Postgres array
// through pgx's database/sql shim is a driver property rather than a
// compile-time one, so both columns are covered by round-trip tests against
// a real server.
func v79ConfidenceMapAndAlarms(tx *sql.Tx) error {
	// ALTER on the partitioned parent cascades to every existing partition.
	for _, t := range []string{"robot_confidence_samples", "robot_confidence_low"} {
		for _, col := range []string{"map_md5 TEXT", "alarm_codes INTEGER[]"} {
			if _, err := tx.Exec(`ALTER TABLE ` + t + ` ADD COLUMN IF NOT EXISTS ` + col); err != nil {
				return fmt.Errorf("v79 confidence %s (%s): %w", col, t, err)
			}
		}
	}
	return nil
}

// v80LaneConfidenceDaily re-keys the per-place aggregate onto the PHYSICAL
// LANE and splits the two populations it was conflating.
//
// THE KEY WAS WRONG, AND IT WAS WRONG BY UP TO A FACTOR OF TWO. scene_edges
// stores every drivable lane twice — 405 directed rows at Springfield are 193
// reciprocal pairs plus 19 genuinely one-way lanes, i.e. 212 pieces of floor.
// Both rows of a pair have identical geometry, so a sample is exactly as close
// to one as to the other and the winner was float noise: 81.7% of samples had
// a second-best directed edge within 5 cm of the best. Aggregating on the
// directed name therefore split each lane's readings arbitrarily between its
// twins — LM73-LM14 showed 48 readings and LM14-LM73 showed 116, one piece of
// floor — so every n, every percentile and every minimum-sample threshold was
// up to 2x wrong, and a lane could fall below the minimum purely because its
// twin took the samples. On the lane key the residual ambiguity is 23.6%, and
// what is left is genuine junction geometry rather than a coin toss.
//
// The old table is DROPPED rather than migrated. Its rows are not wrong, they
// are at the wrong granularity, and there is no arithmetic that recovers one
// lane's distribution from two arbitrary halves of it. The raw samples are
// retained for 14 days precisely so the aggregates can be rebuilt when the
// binning changes — this is the first time that escape hatch has been needed
// and it is the reason it exists.
//
// TWO POPULATIONS, TWO SETS OF COLUMNS, AND THE SPLIT IS THE POINT.
//
//	p05/p25/p50/p75/p95   over EVERY tick, a no-estimate counted as the zero
//	                      it is. Unconditioned, therefore bandable.
//	mean_good/samples_good over only the ticks that produced a number.
//	                      CONDITIONED — the sample was selected by the very
//	                      thing being measured — therefore never bandable.
//
// Getting this wrong is not theoretical: banding the conditioned mean against
// reflector-area membership scored AUC 0.081, i.e. it predicted the dead
// zones almost perfectly BACKWARDS. Lanes running through a reflector-less
// zone average 0.897 against 0.740 elsewhere, because inside them the robot
// produces a good reading or none at all, so what survives is truncated
// rather than degraded. Both columns exist so the gap between them is
// visible, and the gap is the finding.
//
// FIVE PERCENTILES BECAUSE PERCENTILES DO NOT RE-AGGREGATE. A 14-day p05 is
// not the mean of fourteen daily p05s. Keeping only p05 — which is what
// shipped — means the median is gone for every day already rolled up, and no
// amount of later work recovers it. Five doubles on the cheapest tier in the
// system.
//
// sentinel_* and reloc_failed_* are DIFFERENT FACTS and must not share a
// column: at Springfield the vendor's FAILED state has never once fired in
// 10,997 rows, while the no-estimate sentinel is 7.4% of them.
//
// robots_seen because a lane's statistics are a mix over whichever robots
// drove it, and six robots before a change can be six different robots after
// it. It cannot be added retroactively.
//
// version_id is nullable HERE and made NOT NULL when the map sync lands.
// Nothing accumulates in between: this whole body of work is one deploy.
func v80LaneConfidenceDaily(tx *sql.Tx) error {
	stmts := []string{
		`DROP TABLE IF EXISTS segment_confidence_daily`,

		`CREATE TABLE IF NOT EXISTS lane_confidence_daily (
			day                  DATE NOT NULL,
			area_name            TEXT NOT NULL,
			lane                 TEXT NOT NULL,
			p05                  DOUBLE PRECISION,
			p25                  DOUBLE PRECISION,
			p50                  DOUBLE PRECISION,
			p75                  DOUBLE PRECISION,
			p95                  DOUBLE PRECISION,
			samples              INTEGER NOT NULL,
			mean_good            DOUBLE PRECISION,
			samples_good         INTEGER NOT NULL DEFAULT 0,
			min_conf             DOUBLE PRECISION,
			robots               INTEGER NOT NULL,
			robots_seen          TEXT[],
			sentinel_samples     INTEGER NOT NULL DEFAULT 0,
			sentinel_robots      INTEGER NOT NULL DEFAULT 0,
			reloc_failed_samples INTEGER NOT NULL DEFAULT 0,
			reloc_failed_robots  INTEGER NOT NULL DEFAULT 0,
			map_mismatch_samples INTEGER NOT NULL DEFAULT 0,
			version_id           BIGINT,
			PRIMARY KEY (day, area_name, lane)
		)`,

		// The robot row gains the same quarantine count. A robot out of step
		// with the fleet's map is a fact about that robot, and at
		// Hopkinsville it was also sitting undispatchable.
		`ALTER TABLE robot_confidence_daily
		   ADD COLUMN IF NOT EXISTS map_mismatch_samples INTEGER NOT NULL DEFAULT 0`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v80 lane_confidence_daily: %w", err)
		}
	}
	return nil
}

// v81SceneVersioning makes a map edit a first-class event.
//
// THE PROBLEM IT SOLVES. Core has never recorded that the scene changed. It
// mirrors RDS's scene into scene_points/scene_edges by DELETING each area and
// re-inserting it, so the previous state is gone before anything reads it,
// and nothing anywhere carries a hash, a version or a date. The consequence
// is the question this whole line of work exists to answer — "I re-routed
// that lane Tuesday, did it help?" — being unanswerable in principle rather
// than merely unimplemented: there is no before.
//
// FOUR TABLES, TWO TRANSPORTS, ONE TIMELINE.
//
// Lanes come from RDS /scene, gated by its scene_md5. Areas and reflectors
// come from the robot's own .smap, gated by the per-robot current_map_md5.
// Two sources, two gates, two clocks — and an engineer who moved a lane and
// re-drew a reflector zone in the same sitting did ONE edit. scene_diffs is
// the row that relates them: both version streams carry diff_id, so "what
// changed on Tuesday" is one join rather than two timelines somebody has to
// align by eye.
//
// SUPERSEDES_ID IS NOT OPTIONAL, AND max_vertex_delta_m IMPLIES IT.
// Movement is measured FROM something, so the column is not well defined
// without naming the predecessor. The same link is what carries a lane's
// history across a rename: the lane key is a sorted endpoint pair, so
// renaming one point changes the key and orphans everything before it. The
// diff log records the rename; this is the chain a query actually walks.
// NULL means "the first version we ever saw", which is a real state and a
// different one from "unchanged".
//
// WHY A LANE VERSION IS NOT A MAP VERSION. Keying the roll-up to the .smap's
// version would break every lane's series whenever any object anywhere in the
// plant moved — at a daily edit cadence, most lanes most days, which is the
// exact property the magnitude column exists to avoid. It is also the wrong
// provenance: the .smap describes a different artifact on a different
// transport from the travel network the lanes live in.
func v81SceneVersioning(tx *sql.Tx) error {
	stmts := []string{
		// One row per OBSERVED edit. Not per authored edit — nobody is typing
		// anything, and two changes between syncs are one row. previous_sync
		// is therefore load-bearing: it is the window inside which the change
		// happened, and without it "when" is unbounded on the early side.
		`CREATE TABLE IF NOT EXISTS scene_diffs (
			id              BIGSERIAL PRIMARY KEY,
			source          TEXT        NOT NULL,
			gate_hash       TEXT        NOT NULL,
			observed_at     TIMESTAMPTZ NOT NULL,
			previous_sync   TIMESTAMPTZ,
			objects_added   INTEGER     NOT NULL DEFAULT 0,
			objects_changed INTEGER     NOT NULL DEFAULT 0,
			objects_removed INTEGER     NOT NULL DEFAULT 0,
			median_delta_m  DOUBLE PRECISION,
			max_delta_m     DOUBLE PRECISION,
			renames         JSONB
		)`,
		`CREATE INDEX IF NOT EXISTS idx_scene_diffs_observed ON scene_diffs(observed_at DESC)`,

		// Per-lane geometry versions, derived from scene_edges.
		//
		// twins_agree records whether the lane's two directed rows still
		// mirror. All 193 Springfield pairs did on 2026-08-06, including
		// their Bezier handles, so lane grain is safe — and this is what
		// makes the day it stops being safe a query rather than a surprise.
		`CREATE TABLE IF NOT EXISTS scene_lane_versions (
			id                 BIGSERIAL PRIMARY KEY,
			area_name          TEXT NOT NULL,
			lane               TEXT NOT NULL,
			shape_hash         TEXT NOT NULL,
			def_hash           TEXT NOT NULL,
			-- The geometry itself, in canonical direction. A version row that
			-- cannot reproduce the shape it versions cannot answer "how was
			-- this before we touched it", which is one of the four questions
			-- this table exists for; and max_vertex_delta_m is measured FROM
			-- it, so without it the magnitude is only computable at the
			-- instant of the sync and never again. ~100 bytes a row.
			shape              JSONB NOT NULL,
			directed_rows      SMALLINT NOT NULL,
			twins_agree        BOOLEAN NOT NULL DEFAULT TRUE,
			disagreement       TEXT NOT NULL DEFAULT '',
			max_vertex_delta_m DOUBLE PRECISION,
			supersedes_id      BIGINT REFERENCES scene_lane_versions(id),
			diff_id            BIGINT NOT NULL REFERENCES scene_diffs(id),
			valid_from         TIMESTAMPTZ NOT NULL,
			valid_to           TIMESTAMPTZ
		)`,
		// The lookup the roll-up makes per sample: which version was in force
		// on this lane at this instant.
		`CREATE INDEX IF NOT EXISTS idx_scene_lane_versions_current
		   ON scene_lane_versions(area_name, lane, valid_from DESC)`,
		// At most one open version per lane. A second one means a sync wrote
		// a version without closing its predecessor, which would make the
		// temporal lookup ambiguous and is not a state to discover later.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_scene_lane_versions_one_open
		   ON scene_lane_versions(area_name, lane) WHERE valid_to IS NULL`,

		// The .smap archive. BYTEA of gzipped bytes rather than JSONB: this
		// is never queried into — the parsed tables are what queries read —
		// and JSONB stores per-object keys undeduplicated across 264,588
		// scan points, so it would be larger than the text it replaced and
		// cost a parse on every insert. Measured: 7.31 MB of .smap gzips to
		// 1.11 MB.
		//
		// The scan cloud is a SEPARATE column because it is 85-87% of the
		// bytes and the least likely thing anyone asks for. Everything else
		// — areas, reflectors, points, curves, annotation lines — is about
		// 1 MB gzipped at the 5x map, which is a complete history for ~365
		// MB a year. Ageing the cloud out on its own is what makes the byte
		// cap govern the residue instead of the record.
		`CREATE TABLE IF NOT EXISTS scene_map_versions (
			id            BIGSERIAL PRIMARY KEY,
			map_name      TEXT        NOT NULL,
			content_sha   TEXT        NOT NULL,
			map_md5       TEXT        NOT NULL DEFAULT '',
			source_robot  TEXT        NOT NULL DEFAULT '',
			body_gz       BYTEA,
			scan_cloud_gz BYTEA,
			raw_bytes     BIGINT      NOT NULL DEFAULT 0,
			synced_at     TIMESTAMPTZ NOT NULL,
			superseded_at TIMESTAMPTZ,
			diff_id       BIGINT REFERENCES scene_diffs(id),
			UNIQUE (map_name, content_sha)
		)`,

		// Areas, temporally versioned. class_name is the column that replaces
		// reflector_count as the thing anything renders from: measured, the
		// count of reflectors inside a zone has NO predictive power over its
		// no-estimate rate and the sign runs backwards, while every
		// ReflectorArea carrying traffic loses 23-71% of its readings and
		// neither LocConfigArea loses any.
		//
		// reflector_count is still stored — one integer, the input to any
		// future coverage work, and "this declared reflector zone contains
		// zero reflectors" is the most actionable sentence this project has
		// produced. It must not drive a mark, a badge or a band.
		`CREATE TABLE IF NOT EXISTS scene_areas (
			id                 BIGSERIAL PRIMARY KEY,
			area_name          TEXT NOT NULL,
			class_name         TEXT NOT NULL,
			polygon            JSONB NOT NULL,
			reflector_count    INTEGER NOT NULL DEFAULT 0,
			color_pen          BIGINT,
			color_brush        BIGINT,
			properties         JSONB,
			shape_hash         TEXT NOT NULL,
			def_hash           TEXT NOT NULL,
			max_vertex_delta_m DOUBLE PRECISION,
			supersedes_id      BIGINT REFERENCES scene_areas(id),
			diff_id            BIGINT NOT NULL REFERENCES scene_diffs(id),
			map_version_id     BIGINT REFERENCES scene_map_versions(id),
			valid_from         TIMESTAMPTZ NOT NULL,
			valid_to           TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS idx_scene_areas_current
		   ON scene_areas(area_name, valid_from DESC)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_scene_areas_one_open
		   ON scene_areas(area_name) WHERE valid_to IS NULL`,

		// Reflectors, temporally versioned. Identity is the position itself:
		// the vendor gives them no id, and the index in the list is not
		// stable across edits.
		//
		// width is NULLABLE and that is the point — three of Springfield's
		// seventy-one carry no width at all, and 0.0 would claim a
		// zero-width reflector, a measurement nobody made.
		`CREATE TABLE IF NOT EXISTS scene_reflectors (
			id             BIGSERIAL PRIMARY KEY,
			kind           TEXT NOT NULL,
			x              DOUBLE PRECISION NOT NULL,
			y              DOUBLE PRECISION NOT NULL,
			width          DOUBLE PRECISION,
			shape_hash     TEXT NOT NULL,
			supersedes_id  BIGINT REFERENCES scene_reflectors(id),
			diff_id        BIGINT NOT NULL REFERENCES scene_diffs(id),
			map_version_id BIGINT REFERENCES scene_map_versions(id),
			valid_from     TIMESTAMPTZ NOT NULL,
			valid_to       TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS idx_scene_reflectors_current
		   ON scene_reflectors(shape_hash, valid_from DESC)`,

		// The roll-up's link to the geometry it described, and the key change
		// that goes with it.
		//
		// WHY THE DAY IS NO LONGER THE GRAIN. Maps are edited close to daily,
		// so an edit at 14:00 Tuesday produces one Tuesday row mixing six
		// hours of old geometry with ten of new — a blend presented as a
		// measurement, undetectable by any reader. Keying on the version
		// splits that into two rows, each describing one geometry, and the
		// day a lane changed becomes the day it is most readable rather than
		// least.
		//
		// A REAL PRIMARY KEY, because version_id is NOT NULL, because there
		// is no such thing as a reading on a lane with no geometry.
		//
		// An earlier cut of this made version_id nullable and reached for two
		// partial unique indexes to cover it. That was a bandaid over a
		// conflation: a lane's FIRST version was being stamped valid_from =
		// <sync time>, which claims the lane came into existence the moment
		// Core happened to look, leaving every reading taken before that
		// instant unversioned. The fix is at the version, not at the index —
		// a first version opens at -infinity, an open lower bound meaning
		// "the earliest geometry we know of, and we cannot say when it
		// began". Provenance is not lost: when we first SAW it is on the diff
		// row the version points at. valid_from is when it BEGAN; those are
		// two different facts and treating them as one is what produced the
		// nullable column.
		//
		// A lane that has NEVER been versioned is a different matter and is
		// handled where it belongs — the roll-up quarantines those samples
		// and counts them, because "the scene sync has never run" is a defect
		// to surface, not a NULL to carry in a key.
		`ALTER TABLE lane_confidence_daily
		   ADD COLUMN IF NOT EXISTS version_id BIGINT REFERENCES scene_lane_versions(id)`,
		`DELETE FROM lane_confidence_daily WHERE version_id IS NULL`,
		`ALTER TABLE lane_confidence_daily ALTER COLUMN version_id SET NOT NULL`,
		`ALTER TABLE lane_confidence_daily DROP CONSTRAINT IF EXISTS lane_confidence_daily_pkey`,
		`ALTER TABLE lane_confidence_daily
		   ADD CONSTRAINT lane_confidence_daily_pkey
		   PRIMARY KEY (day, area_name, lane, version_id)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v81 scene versioning: %w", err)
		}
	}
	return nil
}
