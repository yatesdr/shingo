package store

import (
	"database/sql"
	"fmt"
)

// tableExists checks if a table exists in the database.
func (db *DB) tableExists(table string) bool {
	switch db.driver {
	case "sqlite":
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		return err == nil
	case "postgres":
		var exists bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name=$1)`, table).Scan(&exists)
		return exists
	}
	return false
}

// columnExists checks if a column exists in a table.
func (db *DB) columnExists(table, column string) bool {
	switch db.driver {
	case "sqlite":
		rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
		if err != nil {
			return false
		}
		defer rows.Close()
		for rows.Next() {
			var cid int
			var name, typ string
			var notnull int
			var dflt sql.NullString
			var pk int
			if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
				return false
			}
			if name == column {
				return true
			}
		}
		return false
	case "postgres":
		var exists bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name=$1 AND column_name=$2)`, table, column).Scan(&exists)
		return exists
	}
	return false
}

// migrateRenames idempotently renames old RDS-specific columns to vendor-neutral names,
// and renames payload_types/payloads to payload_styles/payload_instances (legacy),
// then payload_styles/payload_instances to blueprints/payloads (current).
func (db *DB) migrateRenames() error {
	renames := []struct{ table, oldCol, newCol string }{
		{"orders", "rds_order_id", "vendor_order_id"},
		{"orders", "rds_state", "vendor_state"},
		{"orders", "client_id", "station_id"},
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
	// Rename index idempotently (drop old, new one created by schema)
	if db.driver == "postgres" {
		db.Exec(`DROP INDEX IF EXISTS idx_orders_rds`)
	}

	// Migrate completed -> confirmed status
	db.Exec("UPDATE orders SET status='confirmed' WHERE status='completed'")

	// Rename payload tables: payload_types -> payload_styles, payloads -> payload_instances
	// (legacy rename for very old databases)
	tableRenames := []struct{ oldTable, newTable string }{
		{"payload_types", "payload_styles"},
		{"payloads", "payload_instances"},
		{"node_payload_types", "node_payload_styles"},
	}
	for _, r := range tableRenames {
		if db.tableExists(r.oldTable) && !db.tableExists(r.newTable) {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, r.oldTable, r.newTable)); err != nil {
				return fmt.Errorf("rename table %s: %w", r.oldTable, err)
			}
		}
	}

	// Rename columns in renamed tables (legacy: payload_type_id -> style_id, payload_id -> instance_id)
	colRenames := []struct{ table, oldCol, newCol string }{
		{"payload_instances", "payload_type_id", "style_id"},
		{"node_payload_styles", "payload_type_id", "style_id"},
		{"orders", "payload_type_id", "style_id"},
		{"orders", "payload_id", "instance_id"},
		{"manifest_items", "payload_id", "instance_id"},
		{"corrections", "payload_id", "instance_id"},
	}
	for _, r := range colRenames {
		if db.tableExists(r.table) && db.columnExists(r.table, r.oldCol) {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s RENAME COLUMN %s TO %s`, r.table, r.oldCol, r.newCol)); err != nil {
				return fmt.Errorf("rename %s.%s: %w", r.table, r.oldCol, err)
			}
		}
	}

	return nil
}

// migrate runs schema creation and post-schema migrations.
func (db *DB) migrate() error {
	var schema string
	switch db.driver {
	case "sqlite":
		schema = schemaSQLite
	case "postgres":
		schema = schemaPostgres
	default:
		return fmt.Errorf("no schema for driver: %s", db.driver)
	}
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	if err := db.migrateNodeTypes(); err != nil {
		return fmt.Errorf("migrate node types: %w", err)
	}
	db.migrateShallowLanes()
	db.migratePayloadStyles()
	db.migrateBinsBlueprints()
	db.migrateVendorLocation()
	db.migrateIsSynthetic()
	db.migrateBlueprintDropName()
	db.migrateLegacyCleanup()
	db.migrateDropCapacity()
	db.migrateDropNodeType()
	db.migrateCMSTransactions()
	db.migrateQuantitiesToInteger()
	db.migrateStepsJSON()
	db.migrateBinClaiming()
	db.migrateDeliveryNodeIndex()
	db.migrateStagedBinExpiry()
	return nil
}

// migrateStepsJSON adds the steps_json column to orders for complex multi-step orders.
func (db *DB) migrateStepsJSON() {
	if db.columnExists("orders", "steps_json") {
		return
	}
	db.Exec(`ALTER TABLE orders ADD COLUMN steps_json TEXT NOT NULL DEFAULT ''`)
}

// migrateQuantitiesToInteger converts quantity columns from DOUBLE PRECISION to BIGINT
// on Postgres. SQLite handles this naturally via type affinity.
func (db *DB) migrateQuantitiesToInteger() {
	if db.driver != "postgres" {
		return
	}
	alters := []string{
		`ALTER TABLE orders ALTER COLUMN quantity TYPE BIGINT USING quantity::bigint`,
		`ALTER TABLE corrections ALTER COLUMN quantity TYPE BIGINT USING quantity::bigint`,
		`ALTER TABLE blueprint_manifest ALTER COLUMN quantity TYPE BIGINT USING quantity::bigint`,
		`ALTER TABLE manifest_items ALTER COLUMN quantity TYPE BIGINT USING quantity::bigint`,
		`ALTER TABLE demands ALTER COLUMN demand_qty TYPE BIGINT USING demand_qty::bigint`,
		`ALTER TABLE demands ALTER COLUMN produced_qty TYPE BIGINT USING produced_qty::bigint`,
		`ALTER TABLE production_log ALTER COLUMN quantity TYPE BIGINT USING quantity::bigint`,
	}
	for _, ddl := range alters {
		db.Exec(ddl)
	}
}

// migrateVendorLocation consolidates vendor_location into name and drops the column.
func (db *DB) migrateVendorLocation() {
	if !db.columnExists("nodes", "vendor_location") {
		return
	}
	// Copy vendor_location into name where name is empty but vendor_location is set
	db.Exec(db.Q(`UPDATE nodes SET name = vendor_location WHERE (name = '' OR name IS NULL) AND vendor_location != ''`))

	// SQLite doesn't support DROP COLUMN before 3.35 — use a rebuild.
	// For Postgres, just drop.
	switch db.driver {
	case "sqlite":
		// SQLite 3.35+ supports ALTER TABLE DROP COLUMN.
		db.Exec(`ALTER TABLE nodes DROP COLUMN vendor_location`)
	case "postgres":
		db.Exec(`ALTER TABLE nodes DROP COLUMN IF EXISTS vendor_location`)
	}
}

// migrateIsSynthetic adds the is_synthetic column and populates it from node_types.
func (db *DB) migrateIsSynthetic() {
	if !db.columnExists("nodes", "is_synthetic") {
		db.Exec(`ALTER TABLE nodes ADD COLUMN is_synthetic INTEGER NOT NULL DEFAULT 0`)
	}
	// Populate from node_types for existing rows
	db.Exec(db.Q(`UPDATE nodes SET is_synthetic = 1 WHERE node_type_id IN (SELECT id FROM node_types WHERE is_synthetic = 1) AND is_synthetic = 0`))
}

// migrateBlueprintDropName copies blueprint name to code (where code is empty) and drops the name column.
func (db *DB) migrateBlueprintDropName() {
	if !db.tableExists("blueprints") || !db.columnExists("blueprints", "name") {
		return
	}
	// Copy name into code where code is empty
	db.Exec(db.Q(`UPDATE blueprints SET code = name WHERE code = '' OR code IS NULL`))
	// Drop the name column
	switch db.driver {
	case "sqlite":
		// ALTER TABLE DROP COLUMN requires SQLite 3.35.0+. Try it first,
		// fall back to table recreation for older versions.
		db.Exec(`ALTER TABLE blueprints DROP COLUMN name`)
		if db.columnExists("blueprints", "name") {
			// DROP COLUMN not supported — recreate the table without name
			db.Exec(`CREATE TABLE IF NOT EXISTS blueprints_new (
				id                    INTEGER PRIMARY KEY AUTOINCREMENT,
				code                  TEXT NOT NULL UNIQUE,
				description           TEXT NOT NULL DEFAULT '',
				uop_capacity          INTEGER NOT NULL DEFAULT 0,
				default_manifest_json TEXT NOT NULL DEFAULT '{}',
				created_at            TEXT NOT NULL DEFAULT (datetime('now','localtime')),
				updated_at            TEXT NOT NULL DEFAULT (datetime('now','localtime'))
			)`)
			db.Exec(`INSERT INTO blueprints_new (id, code, description, uop_capacity, default_manifest_json, created_at, updated_at)
				SELECT id, code, description, uop_capacity, COALESCE(default_manifest_json, '{}'), created_at, updated_at FROM blueprints`)
			db.Exec(`DROP TABLE blueprints`)
			db.Exec(`ALTER TABLE blueprints_new RENAME TO blueprints`)
		}
	case "postgres":
		db.Exec(`ALTER TABLE blueprints DROP COLUMN IF EXISTS name`)
	}
}

// migrateLegacyCleanup drops legacy tables and columns from existing databases.
func (db *DB) migrateLegacyCleanup() {
	// Drop legacy tables (safe: IF EXISTS)
	db.Exec(`DROP TABLE IF EXISTS node_inventory`)
	db.Exec(`DROP TABLE IF EXISTS materials`)

	// Drop old table names that were renamed by migrateBinsBlueprints
	// (only if the new tables exist — meaning migration completed successfully)
	if db.tableExists("blueprints") {
		db.Exec(`DROP TABLE IF EXISTS payload_style_manifest`)
		db.Exec(`DROP TABLE IF EXISTS instance_events`)
		// node_payload_styles and payload_instances/payload_styles are renamed,
		// not dropped — they become the new tables. Only drop if old names linger.
		if db.tableExists("payload_styles") && db.tableExists("blueprints") {
			// payload_styles is the old name; blueprints is the new name.
			// If both exist, payload_styles is a leftover (shouldn't happen, but be safe).
		}
	}
}

// migratePayloadStyles adds new columns to payload_styles and payload_instances
// that may not exist if the tables were renamed from payload_types/payloads.
func (db *DB) migratePayloadStyles() {
	// payload_styles new columns
	if db.tableExists("payload_styles") {
		newStyleCols := []struct{ name, def string }{
			{"code", "TEXT NOT NULL DEFAULT ''"},
			{"uop_capacity", "INTEGER NOT NULL DEFAULT 0"},
			{"width_mm", "REAL NOT NULL DEFAULT 0"},
			{"height_mm", "REAL NOT NULL DEFAULT 0"},
			{"depth_mm", "REAL NOT NULL DEFAULT 0"},
			{"weight_kg", "REAL NOT NULL DEFAULT 0"},
		}
		for _, c := range newStyleCols {
			if !db.columnExists("payload_styles", c.name) {
				db.Exec(fmt.Sprintf(`ALTER TABLE payload_styles ADD COLUMN %s %s`, c.name, c.def))
			}
		}
	}

	// payload_instances new columns
	if db.tableExists("payload_instances") {
		newInstanceCols := []struct{ name, def string }{
			{"tag_id", "TEXT NOT NULL DEFAULT ''"},
			{"uop_remaining", "INTEGER NOT NULL DEFAULT 0"},
			{"loaded_at", "TEXT"},
		}
		for _, c := range newInstanceCols {
			if !db.columnExists("payload_instances", c.name) {
				db.Exec(fmt.Sprintf(`ALTER TABLE payload_instances ADD COLUMN %s %s`, c.name, c.def))
			}
		}
	}

	// orders new columns
	if db.tableExists("orders") {
		orderCols := []struct{ name, def string }{
			{"parent_order_id", "INTEGER REFERENCES orders(id)"},
			{"sequence", "INTEGER NOT NULL DEFAULT 0"},
		}
		for _, c := range orderCols {
			if !db.columnExists("orders", c.name) {
				db.Exec(fmt.Sprintf(`ALTER TABLE orders ADD COLUMN %s %s`, c.name, c.def))
			}
		}
	}
}

// migrateBinsBlueprints renames payload_styles -> blueprints, payload_instances -> payloads,
// and related tables/columns. Creates bin_types, bins, and blueprint_bin_types tables.
// Migrates existing data (form_factor -> default bin_type, tag_id/node_id -> bins).
func (db *DB) migrateBinsBlueprints() {
	// ── Step 1: Rename tables ──────────────────────────────────────────
	tableRenames := []struct{ oldTable, newTable string }{
		{"payload_styles", "blueprints"},
		{"payload_instances", "payloads"},
		{"payload_style_manifest", "blueprint_manifest"},
		{"instance_events", "payload_events"},
		{"node_payload_styles", "node_blueprints"},
	}
	for _, r := range tableRenames {
		if db.tableExists(r.oldTable) && !db.tableExists(r.newTable) {
			db.Exec(fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, r.oldTable, r.newTable))
		}
	}

	// ── Step 2: Rename columns ─────────────────────────────────────────
	colRenames := []struct{ table, oldCol, newCol string }{
		// style_id -> blueprint_id
		{"blueprints", "style_id", "blueprint_id"},       // in case leftover from prior rename
		{"payloads", "style_id", "blueprint_id"},          // payload_instances.style_id -> payloads.blueprint_id
		{"blueprint_manifest", "style_id", "blueprint_id"},
		{"orders", "style_id", "blueprint_id"},
		{"node_blueprints", "style_id", "blueprint_id"},
		// instance_id -> payload_id
		{"orders", "instance_id", "payload_id"},
		{"manifest_items", "instance_id", "payload_id"},
		{"corrections", "instance_id", "payload_id"},
		{"payload_events", "instance_id", "payload_id"},
	}
	for _, r := range colRenames {
		if db.tableExists(r.table) && db.columnExists(r.table, r.oldCol) {
			db.Exec(fmt.Sprintf(`ALTER TABLE %s RENAME COLUMN %s TO %s`, r.table, r.oldCol, r.newCol))
		}
	}

	// ── Step 3: Create new tables ──────────────────────────────────────
	if !db.tableExists("bin_types") {
		switch db.driver {
		case "sqlite":
			db.Exec(`CREATE TABLE IF NOT EXISTS bin_types (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				code        TEXT NOT NULL UNIQUE,
				description TEXT NOT NULL DEFAULT '',
				width_in    REAL NOT NULL DEFAULT 0,
				height_in   REAL NOT NULL DEFAULT 0,
				created_at  TEXT NOT NULL DEFAULT (datetime('now','localtime')),
				updated_at  TEXT NOT NULL DEFAULT (datetime('now','localtime'))
			)`)
		case "postgres":
			db.Exec(`CREATE TABLE IF NOT EXISTS bin_types (
				id          BIGSERIAL PRIMARY KEY,
				code        TEXT NOT NULL UNIQUE,
				description TEXT NOT NULL DEFAULT '',
				width_in    DOUBLE PRECISION NOT NULL DEFAULT 0,
				height_in   DOUBLE PRECISION NOT NULL DEFAULT 0,
				created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
			)`)
		}
	}

	if !db.tableExists("bins") {
		switch db.driver {
		case "sqlite":
			db.Exec(`CREATE TABLE IF NOT EXISTS bins (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				bin_type_id INTEGER NOT NULL REFERENCES bin_types(id),
				label       TEXT NOT NULL DEFAULT '',
				description TEXT NOT NULL DEFAULT '',
				node_id     INTEGER REFERENCES nodes(id),
				status      TEXT NOT NULL DEFAULT 'available',
				created_at  TEXT NOT NULL DEFAULT (datetime('now','localtime')),
				updated_at  TEXT NOT NULL DEFAULT (datetime('now','localtime'))
			)`)
			db.Exec(`CREATE INDEX IF NOT EXISTS idx_bins_type ON bins(bin_type_id)`)
			db.Exec(`CREATE INDEX IF NOT EXISTS idx_bins_node ON bins(node_id)`)
			db.Exec(`CREATE INDEX IF NOT EXISTS idx_bins_status ON bins(status)`)
		case "postgres":
			db.Exec(`CREATE TABLE IF NOT EXISTS bins (
				id          BIGSERIAL PRIMARY KEY,
				bin_type_id BIGINT NOT NULL REFERENCES bin_types(id),
				label       TEXT NOT NULL DEFAULT '',
				description TEXT NOT NULL DEFAULT '',
				node_id     BIGINT REFERENCES nodes(id),
				status      TEXT NOT NULL DEFAULT 'available',
				created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
			)`)
			db.Exec(`CREATE INDEX IF NOT EXISTS idx_bins_type ON bins(bin_type_id)`)
			db.Exec(`CREATE INDEX IF NOT EXISTS idx_bins_node ON bins(node_id)`)
			db.Exec(`CREATE INDEX IF NOT EXISTS idx_bins_status ON bins(status)`)
		}
	}

	if !db.tableExists("blueprint_bin_types") {
		switch db.driver {
		case "sqlite":
			db.Exec(`CREATE TABLE IF NOT EXISTS blueprint_bin_types (
				blueprint_id INTEGER NOT NULL REFERENCES blueprints(id) ON DELETE CASCADE,
				bin_type_id  INTEGER NOT NULL REFERENCES bin_types(id) ON DELETE CASCADE,
				PRIMARY KEY (blueprint_id, bin_type_id)
			)`)
		case "postgres":
			db.Exec(`CREATE TABLE IF NOT EXISTS blueprint_bin_types (
				blueprint_id BIGINT NOT NULL REFERENCES blueprints(id) ON DELETE CASCADE,
				bin_type_id  BIGINT NOT NULL REFERENCES bin_types(id) ON DELETE CASCADE,
				PRIMARY KEY (blueprint_id, bin_type_id)
			)`)
		}
	}

	// Add bin_id column to payloads if missing (for databases that were renamed rather than freshly created)
	if db.tableExists("payloads") && !db.columnExists("payloads", "bin_id") {
		switch db.driver {
		case "sqlite":
			db.Exec(`ALTER TABLE payloads ADD COLUMN bin_id INTEGER REFERENCES bins(id)`)
		case "postgres":
			db.Exec(`ALTER TABLE payloads ADD COLUMN bin_id BIGINT REFERENCES bins(id)`)
		}
	}

	// ── Step 4: Data migration ─────────────────────────────────────────
	// Create a DEFAULT bin type from existing form_factor values (if form_factor column still exists)
	db.migrateBinsData()

	// ── Step 5: Drop deprecated columns (silently skip on error) ───────
	// payloads: drop node_id, tag_id, form_factor (moved to bins)
	for _, col := range []string{"node_id", "tag_id", "form_factor"} {
		if db.tableExists("payloads") && db.columnExists("payloads", col) {
			switch db.driver {
			case "sqlite":
				db.Exec(fmt.Sprintf(`ALTER TABLE payloads DROP COLUMN %s`, col))
			case "postgres":
				db.Exec(fmt.Sprintf(`ALTER TABLE payloads DROP COLUMN IF EXISTS %s`, col))
			}
		}
	}

	// blueprints: drop form_factor, width_mm, height_mm, depth_mm, weight_kg (moved to bin_types)
	for _, col := range []string{"form_factor", "width_mm", "height_mm", "depth_mm", "weight_kg"} {
		if db.tableExists("blueprints") && db.columnExists("blueprints", col) {
			switch db.driver {
			case "sqlite":
				db.Exec(fmt.Sprintf(`ALTER TABLE blueprints DROP COLUMN %s`, col))
			case "postgres":
				db.Exec(fmt.Sprintf(`ALTER TABLE blueprints DROP COLUMN IF EXISTS %s`, col))
			}
		}
	}
}

// migrateBinsData creates default bin_type and migrates existing payload data to bins.
func (db *DB) migrateBinsData() {
	if !db.tableExists("bin_types") || !db.tableExists("bins") || !db.tableExists("payloads") {
		return
	}

	// Only create a DEFAULT bin type if there are legacy columns to migrate from
	if !db.columnExists("payloads", "tag_id") && !db.columnExists("payloads", "node_id") {
		return
	}

	// Ensure a DEFAULT bin type exists for migration
	var defaultBinTypeID int64
	err := db.QueryRow(db.Q(`SELECT id FROM bin_types WHERE code = ?`), "DEFAULT").Scan(&defaultBinTypeID)
	if err != nil {
		res, insertErr := db.Exec(db.Q(`INSERT INTO bin_types (code, description) VALUES (?, ?)`), "DEFAULT", "Default bin type (migrated)")
		if insertErr == nil {
			defaultBinTypeID, _ = res.LastInsertId()
			if defaultBinTypeID == 0 {
				// Postgres doesn't support LastInsertId; query it back
				db.QueryRow(db.Q(`SELECT id FROM bin_types WHERE code = ?`), "DEFAULT").Scan(&defaultBinTypeID)
			}
		}
	}
	if defaultBinTypeID == 0 {
		return
	}

	// Find payloads with no bin_id that have a tag_id or node_id to migrate
	type oldPayload struct {
		id    int64
		tagID string
		nodeID sql.NullInt64
	}

	query := `SELECT id`
	if db.columnExists("payloads", "tag_id") {
		query += `, tag_id`
	} else {
		query += `, '' as tag_id`
	}
	if db.columnExists("payloads", "node_id") {
		query += `, node_id`
	} else {
		query += `, NULL as node_id`
	}
	query += ` FROM payloads WHERE bin_id IS NULL`

	rows, err := db.Query(query)
	if err != nil {
		return
	}
	defer rows.Close()

	var payloads []oldPayload
	for rows.Next() {
		var p oldPayload
		if rows.Scan(&p.id, &p.tagID, &p.nodeID) == nil {
			payloads = append(payloads, p)
		}
	}
	rows.Close()

	for _, p := range payloads {
		// Create a bin for this payload
		var binID int64
		if p.nodeID.Valid {
			res, err := db.Exec(db.Q(`INSERT INTO bins (bin_type_id, label, node_id) VALUES (?, ?, ?)`),
				defaultBinTypeID, p.tagID, p.nodeID.Int64)
			if err != nil {
				continue
			}
			binID, _ = res.LastInsertId()
			if binID == 0 {
				db.QueryRow(db.Q(`SELECT id FROM bins WHERE bin_type_id = ? AND label = ? AND node_id = ?`),
					defaultBinTypeID, p.tagID, p.nodeID.Int64).Scan(&binID)
			}
		} else {
			res, err := db.Exec(db.Q(`INSERT INTO bins (bin_type_id, label) VALUES (?, ?)`),
				defaultBinTypeID, p.tagID)
			if err != nil {
				continue
			}
			binID, _ = res.LastInsertId()
			if binID == 0 {
				db.QueryRow(db.Q(`SELECT id FROM bins WHERE bin_type_id = ? AND label = ? AND node_id IS NULL`),
					defaultBinTypeID, p.tagID).Scan(&binID)
			}
		}
		if binID > 0 {
			db.Exec(db.Q(`UPDATE payloads SET bin_id = ? WHERE id = ?`), binID, p.id)
		}
	}
}

// migrateNodeTypes adds node_type_id and parent_id columns to nodes (if missing)
// and seeds default node types.
func (db *DB) migrateNodeTypes() error {
	if !db.columnExists("nodes", "node_type_id") {
		if _, err := db.Exec(`ALTER TABLE nodes ADD COLUMN node_type_id INTEGER REFERENCES node_types(id)`); err != nil {
			return fmt.Errorf("add node_type_id: %w", err)
		}
	}
	if !db.columnExists("nodes", "parent_id") {
		if _, err := db.Exec(`ALTER TABLE nodes ADD COLUMN parent_id INTEGER REFERENCES nodes(id)`); err != nil {
			return fmt.Errorf("add parent_id: %w", err)
		}
	}

	// Rename old node type codes to canonical codes
	for _, rename := range [][2]string{
		{"SUP", "SMKT"}, {"LAN", "LANE"}, {"SHF", "SHUF"},
		{"CHG", "CHRG"}, {"OFL", "OVFL"}, {"STN", "STAG"},
		{"SMKT", "NGRP"},
	} {
		db.Exec(db.Q(`UPDATE node_types SET code=? WHERE code=?`), rename[1], rename[0])
	}

	// Remove legacy Storage type — reassign any nodes using it to nil
	db.Exec(db.Q(`UPDATE nodes SET node_type_id = NULL WHERE node_type_id IN (SELECT id FROM node_types WHERE code = 'STG')`))
	db.Exec(db.Q(`DELETE FROM node_types WHERE code = 'STG'`))

	// Only structural (synthetic) types are needed — physical nodes don't require a type.
	seeds := []struct {
		code, name, desc string
	}{
		{"LANE", "Lane", "Lane (groups depth-ordered slots)"},
		{"NGRP", "Node Group", "Node group (synthetic parent for lanes and direct nodes)"},
	}
	for _, s := range seeds {
		db.Exec(db.Q(`INSERT INTO node_types (code, name, description, is_synthetic) VALUES (?, ?, ?, 1) ON CONFLICT (code) DO NOTHING`),
			s.code, s.name, s.desc)
	}

	// Clear node_type_id from physical nodes — types are only for synthetic nodes
	db.Exec(db.Q(`UPDATE nodes SET node_type_id = NULL WHERE node_type_id IN (SELECT id FROM node_types WHERE is_synthetic = 0)`))

	// Remove legacy SHUF type — reassign any SHUF nodes to LANE
	var laneTypeID int64
	if row := db.QueryRow(db.Q(`SELECT id FROM node_types WHERE code='LANE'`)); row != nil {
		row.Scan(&laneTypeID)
	}
	if laneTypeID > 0 {
		db.Exec(db.Q(`UPDATE nodes SET node_type_id = ? WHERE node_type_id IN (SELECT id FROM node_types WHERE code = 'SHUF')`), laneTypeID)
	}
	db.Exec(db.Q(`DELETE FROM node_types WHERE code = 'SHUF'`))

	return nil
}

// migrateShallowLanes dissolves shallow lanes into direct children of the parent group.
// Finds LANE nodes with shallow=true property, reparents their physical children
// to the grandparent NGRP, and deletes the empty shallow lane nodes.
func (db *DB) migrateShallowLanes() {
	// Find all LANE nodes with shallow=true property
	rows, err := db.Query(db.Q(`SELECT np.node_id FROM node_properties np JOIN nodes n ON n.id = np.node_id WHERE np.key = 'shallow' AND np.value = 'true'`))
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

	// Determine which table name to use for node-blueprint associations
	nbTable := "node_blueprints"
	if !db.tableExists("node_blueprints") && db.tableExists("node_payload_styles") {
		nbTable = "node_payload_styles"
	}

	for _, laneID := range shallowLaneIDs {
		lane, err := db.GetNode(laneID)
		if err != nil || lane.ParentID == nil {
			continue
		}
		groupID := *lane.ParentID

		// Reparent physical children to the group
		children, _ := db.ListChildNodes(laneID)
		for _, child := range children {
			if !child.IsSynthetic {
				db.Exec(db.Q(`UPDATE nodes SET parent_id=?, updated_at=datetime('now','localtime') WHERE id=?`), groupID, child.ID)
				db.DeleteNodeProperty(child.ID, "depth")
				db.DeleteNodeProperty(child.ID, "role")
			}
		}

		// Delete the shallow lane node
		db.Exec(db.Q(`DELETE FROM node_properties WHERE node_id=?`), laneID)
		db.Exec(db.Q(`DELETE FROM node_stations WHERE node_id=?`), laneID)
		db.Exec(db.Q(fmt.Sprintf(`DELETE FROM %s WHERE node_id=?`, nbTable)), laneID)
		db.Exec(db.Q(`DELETE FROM nodes WHERE id=?`), laneID)
	}
}

// migrateDropNodeType removes the legacy node_type free-text column from nodes.
// Physical nodes no longer need a type; structural nodes use node_type_id (FK to node_types).
func (db *DB) migrateDropNodeType() {
	if !db.columnExists("nodes", "node_type") {
		return
	}
	switch db.driver {
	case "sqlite":
		db.Exec(`ALTER TABLE nodes DROP COLUMN node_type`)
	case "postgres":
		db.Exec(`ALTER TABLE nodes DROP COLUMN IF EXISTS node_type`)
	}
}

// migrateCMSTransactions alters the cms_transactions table from old schema
// (direction/quantity) to new schema (txn_type/delta/qty_before/qty_after/bin_label).
func (db *DB) migrateCMSTransactions() {
	if !db.tableExists("cms_transactions") {
		return
	}
	if db.columnExists("cms_transactions", "txn_type") {
		return // already has new schema
	}

	// Rename direction -> txn_type
	if db.columnExists("cms_transactions", "direction") {
		db.Exec(fmt.Sprintf(`ALTER TABLE cms_transactions RENAME COLUMN direction TO txn_type`))
	}

	// Rename quantity -> delta (REAL -> stored as INTEGER going forward, existing floats are fine)
	if db.columnExists("cms_transactions", "quantity") {
		db.Exec(fmt.Sprintf(`ALTER TABLE cms_transactions RENAME COLUMN quantity TO delta`))
	}

	// Add missing columns
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

// migrateBinClaiming adds claimed_by to bins and bin_id to orders for bin-centric dispatch.
func (db *DB) migrateBinClaiming() {
	if !db.columnExists("bins", "claimed_by") {
		switch db.driver {
		case "sqlite":
			db.Exec(`ALTER TABLE bins ADD COLUMN claimed_by INTEGER REFERENCES orders(id)`)
		case "postgres":
			db.Exec(`ALTER TABLE bins ADD COLUMN claimed_by BIGINT REFERENCES orders(id)`)
		}
	}
	if !db.columnExists("orders", "bin_id") {
		switch db.driver {
		case "sqlite":
			db.Exec(`ALTER TABLE orders ADD COLUMN bin_id INTEGER REFERENCES bins(id)`)
		case "postgres":
			db.Exec(`ALTER TABLE orders ADD COLUMN bin_id BIGINT REFERENCES bins(id)`)
		}
	}
}

// migrateStagedBinExpiry adds staged_at and staged_expires_at columns to bins.
func (db *DB) migrateStagedBinExpiry() {
	if !db.columnExists("bins", "staged_at") {
		switch db.driver {
		case "sqlite":
			db.Exec(`ALTER TABLE bins ADD COLUMN staged_at TEXT`)
		case "postgres":
			db.Exec(`ALTER TABLE bins ADD COLUMN staged_at TIMESTAMPTZ`)
		}
	}
	if !db.columnExists("bins", "staged_expires_at") {
		switch db.driver {
		case "sqlite":
			db.Exec(`ALTER TABLE bins ADD COLUMN staged_expires_at TEXT`)
		case "postgres":
			db.Exec(`ALTER TABLE bins ADD COLUMN staged_expires_at TIMESTAMPTZ`)
		}
	}
}

// migrateDeliveryNodeIndex adds an index on orders.delivery_node for in-flight delivery counting.
func (db *DB) migrateDeliveryNodeIndex() {
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_orders_delivery_node ON orders(delivery_node)`)
}

// migrateDropCapacity removes the capacity column from nodes (all nodes now have capacity 1).
func (db *DB) migrateDropCapacity() {
	if !db.columnExists("nodes", "capacity") {
		return
	}
	switch db.driver {
	case "sqlite":
		db.Exec(`ALTER TABLE nodes DROP COLUMN capacity`)
	case "postgres":
		db.Exec(`ALTER TABLE nodes DROP COLUMN IF EXISTS capacity`)
	}
}
