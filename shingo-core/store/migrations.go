package store

import "fmt"

// tableExists checks if a table exists in the database.
func (db *DB) tableExists(table string) bool {
	var exists bool
	db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name=$1)`, table).Scan(&exists)
	return exists
}

// columnExists checks if a column exists in a table.
func (db *DB) columnExists(table, column string) bool {
	var exists bool
	db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name=$1 AND column_name=$2)`, table, column).Scan(&exists)
	return exists
}

// isColumnType returns the data type of a column, or empty string if not found.
func (db *DB) isColumnType(table, column string) string {
	var dataType string
	db.QueryRow(
		`SELECT data_type FROM information_schema.columns WHERE table_name=$1 AND column_name=$2`,
		table, column,
	).Scan(&dataType)
	return dataType
}

// migrateRenames idempotently renames old columns to vendor-neutral names.
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
		if db.columnExists(r.table, r.oldCol) {
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

// migrate runs column renames (for ancient databases), schema creation, and versioned migrations.
func (db *DB) migrate() error {
	// Renames must run before schema creation since they fix column names
	// on tables that CREATE TABLE IF NOT EXISTS would skip.
	if err := db.migrateRenames(); err != nil {
		return fmt.Errorf("migrate renames: %w", err)
	}
	if _, err := db.Exec(schemaPostgres); err != nil {
		return fmt.Errorf("schema exec: %w", err)
	}
	return db.runVersionedMigrations()
}

// runVersionedMigrations runs numbered migrations that are tracked in a
// schema_migrations table. Each migration runs exactly once.
func (db *DB) runVersionedMigrations() error {
	db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)

	migrations := []struct {
		version int
		name    string
		fn      func() error
	}{
		{1, "convert boolean columns to native BOOLEAN", db.v1BooleanColumns},
		{2, "add depth column to nodes", db.v2DepthColumn},
		{3, "drop dead columns", db.v3DropDeadColumns},
		{4, "drop vestigial payload_id from orders", db.v4DropOrderPayloadID},
		{5, "backfill mission telemetry for completed orders", db.v5MissionTelemetryBackfill},
		{6, "consolidate legacy migrations", db.v6LegacyConsolidation},
		{7, "drop vestigial default_manifest_json from payloads", db.v7DropDefaultManifestJSON},
		{8, "add payload_code column to orders", db.v8OrderPayloadCode},
		{9, "create order_bins junction table for multi-bin complex orders", db.v9OrderBins},
	}

	for _, m := range migrations {
		var exists bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, m.version).Scan(&exists)
		if exists {
			continue
		}
		if err := m.fn(); err != nil {
			return fmt.Errorf("migration v%d (%s): %w", m.version, m.name, err)
		}
		db.Exec(`INSERT INTO schema_migrations (version) VALUES ($1)`, m.version)
	}
	return nil
}

// v1BooleanColumns converts INTEGER boolean columns to native BOOLEAN.
func (db *DB) v1BooleanColumns() error {
	conversions := []struct{ table, column, defVal string }{
		{"nodes", "is_synthetic", "false"},
		{"nodes", "enabled", "true"},
		{"node_types", "is_synthetic", "false"},
		{"bins", "manifest_confirmed", "false"},
		{"bins", "locked", "false"},
	}
	for _, c := range conversions {
		if !db.tableExists(c.table) || !db.columnExists(c.table, c.column) {
			continue
		}
		if db.isColumnType(c.table, c.column) == "boolean" {
			continue
		}
		db.Exec(fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT`, c.table, c.column))
		db.Exec(fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s TYPE BOOLEAN USING %s != 0`, c.table, c.column, c.column))
		db.Exec(fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s`, c.table, c.column, c.defVal))
	}
	db.Exec(`DROP INDEX IF EXISTS idx_bins_locked`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_bins_locked ON bins(locked) WHERE locked = true`)
	return nil
}

// v2DepthColumn adds a depth column to nodes and migrates data from node_properties.
func (db *DB) v2DepthColumn() error {
	if db.columnExists("nodes", "depth") {
		return nil
	}
	db.Exec(`ALTER TABLE nodes ADD COLUMN depth INTEGER`)
	db.Exec(`UPDATE nodes SET depth = CAST(np.value AS INTEGER)
		FROM node_properties np
		WHERE np.node_id = nodes.id AND np.key = 'depth'`)
	db.Exec(`DELETE FROM node_properties WHERE key = 'depth'`)
	return nil
}

// v3DropDeadColumns removes columns that are no longer used.
func (db *DB) v3DropDeadColumns() error {
	drops := []struct{ table, column string }{
		{"orders", "source_node_id"},
		{"orders", "dest_node_id"},
		{"orders", "factory_id"},
		{"edge_registry", "factory_id"},
	}
	for _, d := range drops {
		if db.columnExists(d.table, d.column) {
			db.Exec(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN IF EXISTS %s`, d.table, d.column))
		}
	}
	return nil
}

// v4DropOrderPayloadID removes the vestigial payload_id column from orders.
// Orders now reference bins via bin_id; the payload template is accessed through
// the bin's payload_code field.
func (db *DB) v4DropOrderPayloadID() error {
	if db.columnExists("orders", "payload_id") {
		db.Exec(`ALTER TABLE orders DROP COLUMN IF EXISTS payload_id`)
	}
	return nil
}

// v7DropDefaultManifestJSON removes the vestigial default_manifest_json column
// from payloads. Manifest templates are stored in the normalized payload_manifest
// table; this denormalized blob was never read at runtime.
func (db *DB) v7DropDefaultManifestJSON() error {
	if db.columnExists("payloads", "default_manifest_json") {
		db.Exec(`ALTER TABLE payloads DROP COLUMN default_manifest_json`)
	}
	return nil
}

// v6LegacyConsolidation runs all legacy (previously unversioned) migrations once.
// Each sub-migration is idempotent to handle databases of any age.
func (db *DB) v6LegacyConsolidation() error {
	if err := db.migrateNodeTypes(); err != nil {
		return fmt.Errorf("node types: %w", err)
	}
	db.migrateShallowLanes()
	db.migrateVendorLocation()
	db.migrateIsSynthetic()
	db.migrateDropCapacity()
	db.migrateDropNodeType()
	db.migrateCMSTransactions()
	db.migrateStepsJSON()
	db.migrateBinClaiming()
	db.migrateDeliveryNodeIndex()
	db.migrateBinsCommandCenter()
	return nil
}

// --- Legacy migrations (idempotent, retained for v6 consolidation) ---

func (db *DB) migrateStepsJSON() {
	if db.columnExists("orders", "steps_json") {
		return
	}
	db.Exec(`ALTER TABLE orders ADD COLUMN steps_json TEXT NOT NULL DEFAULT ''`)
}

func (db *DB) migrateVendorLocation() {
	if !db.columnExists("nodes", "vendor_location") {
		return
	}
	db.Exec(`UPDATE nodes SET name = vendor_location WHERE (name = '' OR name IS NULL) AND vendor_location != ''`)
	db.Exec(`ALTER TABLE nodes DROP COLUMN IF EXISTS vendor_location`)
}

func (db *DB) migrateIsSynthetic() {
	if !db.columnExists("nodes", "is_synthetic") {
		db.Exec(`ALTER TABLE nodes ADD COLUMN is_synthetic BOOLEAN NOT NULL DEFAULT false`)
	}
	db.Exec(`UPDATE nodes SET is_synthetic = true WHERE node_type_id IN (SELECT id FROM node_types WHERE is_synthetic = true) AND is_synthetic = false`)
}

func (db *DB) migrateDropCapacity() {
	if !db.columnExists("nodes", "capacity") {
		return
	}
	db.Exec(`ALTER TABLE nodes DROP COLUMN IF EXISTS capacity`)
}

func (db *DB) migrateDropNodeType() {
	if !db.columnExists("nodes", "node_type") {
		return
	}
	db.Exec(`ALTER TABLE nodes DROP COLUMN IF EXISTS node_type`)
}

func (db *DB) migrateCMSTransactions() {
	if !db.tableExists("cms_transactions") {
		return
	}
	if db.columnExists("cms_transactions", "txn_type") {
		return
	}
	if db.columnExists("cms_transactions", "direction") {
		db.Exec(`ALTER TABLE cms_transactions RENAME COLUMN direction TO txn_type`)
	}
	if db.columnExists("cms_transactions", "quantity") {
		db.Exec(`ALTER TABLE cms_transactions RENAME COLUMN quantity TO delta`)
	}
	newCols := []struct{ name, def string }{
		{"qty_before", "INTEGER NOT NULL DEFAULT 0"},
		{"qty_after", "INTEGER NOT NULL DEFAULT 0"},
		{"bin_label", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, c := range newCols {
		if !db.columnExists("cms_transactions", c.name) {
			db.Exec(fmt.Sprintf(`ALTER TABLE cms_transactions ADD COLUMN %s %s`, c.name, c.def))
		}
	}
}

func (db *DB) migrateBinClaiming() {
	if !db.columnExists("bins", "claimed_by") {
		db.Exec(`ALTER TABLE bins ADD COLUMN claimed_by BIGINT REFERENCES orders(id)`)
	}
	if !db.columnExists("orders", "bin_id") {
		db.Exec(`ALTER TABLE orders ADD COLUMN bin_id BIGINT REFERENCES bins(id)`)
	}
}

func (db *DB) migrateDeliveryNodeIndex() {
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_orders_delivery_node ON orders(delivery_node)`)
}

func (db *DB) migrateBinsCommandCenter() {
	cols := []struct{ name, def string }{
		{"locked", "BOOLEAN NOT NULL DEFAULT false"},
		{"locked_by", "TEXT NOT NULL DEFAULT ''"},
		{"locked_at", "TIMESTAMPTZ"},
		{"last_counted_at", "TIMESTAMPTZ"},
		{"last_counted_by", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, c := range cols {
		if db.columnExists("bins", c.name) {
			continue
		}
		db.Exec(fmt.Sprintf(`ALTER TABLE bins ADD COLUMN %s %s`, c.name, c.def))
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_bins_locked ON bins(locked) WHERE locked = true`)
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_bins_label_unique ON bins(label) WHERE label != ''`)
}

func (db *DB) migrateNodeTypes() error {
	if !db.columnExists("nodes", "node_type_id") {
		if _, err := db.Exec(`ALTER TABLE nodes ADD COLUMN node_type_id BIGINT`); err != nil {
			return fmt.Errorf("add node_type_id: %w", err)
		}
	}
	if !db.columnExists("nodes", "parent_id") {
		if _, err := db.Exec(`ALTER TABLE nodes ADD COLUMN parent_id BIGINT REFERENCES nodes(id)`); err != nil {
			return fmt.Errorf("add parent_id: %w", err)
		}
	}

	for _, rename := range [][2]string{
		{"SUP", "SMKT"}, {"LAN", "LANE"}, {"SHF", "SHUF"},
		{"CHG", "CHRG"}, {"OFL", "OVFL"}, {"STN", "STAG"},
		{"SMKT", "NGRP"},
	} {
		db.Exec(`UPDATE node_types SET code=$1 WHERE code=$2`, rename[1], rename[0])
	}

	db.Exec(`UPDATE nodes SET node_type_id = NULL WHERE node_type_id IN (SELECT id FROM node_types WHERE code = 'STG')`)
	db.Exec(`DELETE FROM node_types WHERE code = 'STG'`)

	seeds := []struct{ code, name, desc string }{
		{"LANE", "Lane", "Lane (groups depth-ordered slots)"},
		{"NGRP", "Node Group", "Node group (synthetic parent for lanes and direct nodes)"},
	}
	for _, s := range seeds {
		db.Exec(`INSERT INTO node_types (code, name, description, is_synthetic) VALUES ($1, $2, $3, true) ON CONFLICT (code) DO NOTHING`,
			s.code, s.name, s.desc)
	}

	db.Exec(`UPDATE nodes SET node_type_id = NULL WHERE node_type_id IN (SELECT id FROM node_types WHERE is_synthetic = false)`)

	var laneTypeID int64
	if row := db.QueryRow(`SELECT id FROM node_types WHERE code='LANE'`); row != nil {
		row.Scan(&laneTypeID)
	}
	if laneTypeID > 0 {
		db.Exec(`UPDATE nodes SET node_type_id = $1 WHERE node_type_id IN (SELECT id FROM node_types WHERE code = 'SHUF')`, laneTypeID)
	}
	db.Exec(`DELETE FROM node_types WHERE code = 'SHUF'`)

	return nil
}

func (db *DB) migrateShallowLanes() {
	rows, err := db.Query(`SELECT np.node_id FROM node_properties np JOIN nodes n ON n.id = np.node_id WHERE np.key = 'shallow' AND np.value = 'true'`)
	if err != nil {
		return
	}
	defer rows.Close()

	var shallowLaneIDs []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			shallowLaneIDs = append(shallowLaneIDs, id)
		}
	}

	for _, laneID := range shallowLaneIDs {
		lane, err := db.GetNode(laneID)
		if err != nil || lane.ParentID == nil {
			continue
		}
		groupID := *lane.ParentID

		children, _ := db.ListChildNodes(laneID)
		for _, child := range children {
			if !child.IsSynthetic {
				db.Exec(`UPDATE nodes SET parent_id=$1, updated_at=NOW() WHERE id=$2`, groupID, child.ID)
				db.DeleteNodeProperty(child.ID, "role")
			}
		}

		db.Exec(`DELETE FROM node_properties WHERE node_id=$1`, laneID)
		db.Exec(`DELETE FROM node_stations WHERE node_id=$1`, laneID)
		db.Exec(`DELETE FROM node_payloads WHERE node_id=$1`, laneID)
		db.Exec(`DELETE FROM nodes WHERE id=$1`, laneID)
	}
}

// v8OrderPayloadCode adds payload_code column to orders for queued order fulfillment.
func (db *DB) v8OrderPayloadCode() error {
	if !db.columnExists("orders", "payload_code") {
		_, err := db.Exec(`ALTER TABLE orders ADD COLUMN payload_code TEXT NOT NULL DEFAULT ''`)
		return err
	}
	return nil
}

// v9OrderBins creates the order_bins junction table for multi-bin complex order tracking.
// Single-bin orders continue using Order.BinID. Multi-pickup complex orders record
// per-bin destinations so handleOrderCompleted can move each bin to the correct node.
func (db *DB) v9OrderBins() error {
	if db.tableExists("order_bins") {
		return nil
	}
	_, err := db.Exec(`CREATE TABLE order_bins (
		id          BIGSERIAL PRIMARY KEY,
		order_id    BIGINT NOT NULL REFERENCES orders(id),
		bin_id      BIGINT NOT NULL REFERENCES bins(id),
		step_index  INT NOT NULL,
		action      TEXT NOT NULL,
		node_name   TEXT NOT NULL,
		dest_node   TEXT NOT NULL DEFAULT '',
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	if err != nil {
		return fmt.Errorf("create order_bins table: %w", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_order_bins_order ON order_bins(order_id)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_order_bins_bin ON order_bins(bin_id)`)
	return nil
}

// v5MissionTelemetryBackfill creates summary rows for historical completed orders.
func (db *DB) v5MissionTelemetryBackfill() error {
	db.Exec(`INSERT INTO mission_telemetry
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
	return nil
}
