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
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_lineside_buckets_node_style ON lineside_buckets(node_id, style_id)`); err != nil {
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
