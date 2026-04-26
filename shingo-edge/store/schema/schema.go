// Package schema holds shingo-edge's SQLite baseline DDL and
// pragma_table_info / sqlite_master introspection helpers.
//
// Phase 6.0b of the architecture refactor cut this seam out of the
// 1013-line store/schema.go, which previously bundled the canonical
// CREATE TABLE constant, the cleanup-DROP constant, the migrate()
// entry point, ~17 versioned per-column rebuilds, and the
// introspection methods all in a single file. After 6.0b the split
// is:
//
//   store/schema/sqlite_ddl.go — sqliteDDL constant (canonical state)
//   store/schema/schema.go     — Apply / TableExists / TableHasColumn
//   store/migrations.go        — migrate(), legacy-rename and per-table
//                                rebuild helpers
//
// All helpers take *sql.DB so they're usable without the outer
// *store.DB type, keeping the dependency graph one-way:
// store -> store/schema, never the reverse.
//
// SQLite quirk to remember: `CREATE TABLE IF NOT EXISTS` and
// `CREATE INDEX IF NOT EXISTS` are both idempotent, so Apply is
// safe to run on a fresh DB, an in-flight DB, or a fully-migrated
// DB. Versioned column-level migrations (renames, rebuilds, ALTER
// ADD COLUMN) live in store/migrations.go and run *after* Apply.
package schema

import (
	"database/sql"
	"fmt"
)

// Apply executes the canonical schema DDL, creating every table and
// index idempotently. Apply does NOT run the legacy-table cleanup
// DROPs or the per-version column rebuilds — those live in the
// migrate() entry point in store/migrations.go and must be ordered
// around the Apply call (drops + renames before, ALTERs + fixups
// after) to preserve data on databases of any age.
func Apply(db *sql.DB) error {
	if _, err := db.Exec(sqliteDDL); err != nil {
		return fmt.Errorf("schema apply: %w", err)
	}
	return nil
}

// TableHasColumn reports whether the named column exists on the named
// table. Returns (false, nil) for missing tables; the caller treats a
// missing table as "no, this column doesn't exist", which matches
// every existing call-site after the migrations.go extraction.
func TableHasColumn(db *sql.DB, tableName, columnName string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('`+tableName+`') WHERE name = ?`, columnName)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

// TableExists reports whether the named table is present in
// sqlite_master. Returns (false, nil) for unknown table names rather
// than an error, mirroring the pre-6.0b method behavior on *DB.
func TableExists(db *sql.DB, tableName string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tableName)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}
