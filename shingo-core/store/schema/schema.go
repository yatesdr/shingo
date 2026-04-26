// Package schema holds shingo-core's PostgreSQL baseline DDL and
// information_schema introspection helpers.
//
// Phase 6.0a of the architecture refactor cut this seam out of
// store/schema_postgres.go (DDL constant) and store/migrations.go
// (per-DB introspection methods on *store.DB). The DDL is applied
// once via Apply() before the versioned migration loop in
// store.runVersionedMigrations() runs; introspection helpers are
// used by per-version migration funcs to make schema changes
// idempotent across DBs of any age.
//
// All helpers take *sql.DB so they're usable without depending on
// the outer *store.DB type, keeping the dependency graph one-way:
// store -> store/schema, never the reverse.
package schema

import (
	"database/sql"
	"fmt"
)

// Apply executes the baseline DDL, creating all tables and indexes.
// Every statement uses CREATE ... IF NOT EXISTS, so Apply is safe to
// run on a fresh database or on an existing one — it never destroys
// data and never errors on already-present tables.
//
// Versioned migrations (in store/migrations.go) run after Apply and
// handle column-level evolution.
func Apply(db *sql.DB) error {
	if _, err := db.Exec(postgresDDL); err != nil {
		return fmt.Errorf("schema apply: %w", err)
	}
	return nil
}

// TableExists reports whether the named table exists in the database's
// public schema. Returns false on any query error (the table also
// "doesn't exist" if we can't read information_schema).
func TableExists(db *sql.DB, table string) bool {
	var exists bool
	db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name=$1)`,
		table,
	).Scan(&exists)
	return exists
}

// ColumnExists reports whether the named column exists on the named
// table. Returns false on any query error or if either name is empty.
func ColumnExists(db *sql.DB, table, column string) bool {
	var exists bool
	db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name=$1 AND column_name=$2)`,
		table, column,
	).Scan(&exists)
	return exists
}

// ColumnType returns the SQL data type of the named column (e.g.
// "boolean", "integer", "text"), or an empty string if the column
// does not exist or any query error occurs.
func ColumnType(db *sql.DB, table, column string) string {
	var dataType string
	db.QueryRow(
		`SELECT data_type FROM information_schema.columns WHERE table_name=$1 AND column_name=$2`,
		table, column,
	).Scan(&dataType)
	return dataType
}
