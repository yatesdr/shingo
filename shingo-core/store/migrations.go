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
// and renames payload_types/payloads to payload_styles/payload_instances.
func (db *DB) migrateRenames() error {
	renames := []struct{ table, oldCol, newCol string }{
		{"nodes", "rds_location", "vendor_location"},
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

	// Rename columns in renamed tables
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
	db.migratePayloadStyles()
	db.migrateLegacyCleanup()
	return nil
}

// migrateLegacyCleanup drops legacy tables and columns from existing databases.
func (db *DB) migrateLegacyCleanup() {
	// Drop legacy tables (safe: IF EXISTS)
	db.Exec(`DROP TABLE IF EXISTS node_inventory`)
	db.Exec(`DROP TABLE IF EXISTS materials`)
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

	seeds := []struct {
		code, name, desc string
		synthetic        bool
	}{
		{"STG", "Storage", "General storage location", false},
		{"LSL", "Lineside", "Line-side delivery point", false},
		{"SUP", "Supermarket", "Supermarket zone (synthetic parent)", true},
		{"OFL", "Overflow", "Overflow storage area", false},
		{"STN", "Staging", "Staging area", false},
		{"CHG", "Charging", "Robot charging station", false},
		{"LAN", "Lane", "Supermarket lane (groups depth-ordered slots)", true},
		{"SHF", "Shuffle Row", "Temporary shuffle staging row", true},
	}
	for _, s := range seeds {
		synVal := 0
		if s.synthetic {
			synVal = 1
		}
		db.Exec(db.Q(`INSERT INTO node_types (code, name, description, is_synthetic) VALUES (?, ?, ?, ?) ON CONFLICT (code) DO NOTHING`),
			s.code, s.name, s.desc, synVal)
	}

	typeMap := map[string]string{
		"storage":   "STG",
		"line_side": "LSL",
		"staging":   "STN",
		"charging":  "CHG",
	}
	for textType, code := range typeMap {
		db.Exec(db.Q(`UPDATE nodes SET node_type_id = (SELECT id FROM node_types WHERE code = ?) WHERE node_type = ? AND node_type_id IS NULL`), code, textType)
	}

	return nil
}
