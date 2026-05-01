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

// migrate runs column renames (for ancient databases), the baseline DDL via
// the schema sub-package, then versioned migrations. Order matters: renames
// fix tables that the baseline CREATE ... IF NOT EXISTS would otherwise skip.
func (db *DB) migrate() error {
	if err := db.migrateRenames(); err != nil {
		return fmt.Errorf("migrate renames: %w", err)
	}
	if err := schema.Apply(db.DB); err != nil {
		return err
	}
	return db.runVersionedMigrations()
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
