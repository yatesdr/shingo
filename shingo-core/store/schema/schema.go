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
// Helpers accept the Querier interface so they work equally well
// with *sql.DB (the connection pool) and *sql.Tx (an in-flight
// transaction). Migrations run inside a per-version transaction so
// the migration's DDL and the schema_migrations row insert commit
// or roll back together — without that, an ALTER TABLE that fails
// midway can still leave behind a version row that fools the runner
// into thinking the migration succeeded. (See store.runVersionedMigrations.)
package schema

import (
	"database/sql"
	"fmt"
	"strings"
)

// Querier is the subset of database/sql methods these helpers need.
// *sql.DB and *sql.Tx both satisfy it via Go's structural typing,
// which lets migration code use the same helpers whether it's
// running against the connection pool or inside a transaction.
type Querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

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
func TableExists(c Querier, table string) bool {
	var exists bool
	c.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name=$1)`,
		table,
	).Scan(&exists)
	return exists
}

// ColumnExists reports whether the named column exists on the named
// table. Returns false on any query error or if either name is empty.
// CheckConstraintAllows reports whether a named CHECK constraint's definition
// mentions `value`. It is the verify hook for migrations that WIDEN a CHECK
// (adding a permitted enum value), which — unlike ADD COLUMN IF NOT EXISTS — has
// no idempotent DDL form and so must be drop-and-recreate. Substring matching on
// pg_get_constraintdef is coarse, and that is fine for the job: the question is
// only "has the wider constraint been installed", and a false negative merely
// re-applies an idempotent drop-and-recreate.
func CheckConstraintAllows(c Querier, table, constraint, value string) bool {
	var def string
	if err := c.QueryRow(
		`SELECT pg_get_constraintdef(pgc.oid)
		 FROM pg_constraint pgc
		 JOIN pg_class rel ON rel.oid = pgc.conrelid
		 WHERE rel.relname = $1 AND pgc.conname = $2`,
		table, constraint,
	).Scan(&def); err != nil {
		return false
	}
	return strings.Contains(def, value)
}

func ColumnExists(c Querier, table, column string) bool {
	var exists bool
	c.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name=$1 AND column_name=$2)`,
		table, column,
	).Scan(&exists)
	return exists
}

// ColumnAbsent and IndexAbsent are the ABSENCE forms of the Exists helpers
// below, and they exist because negating those is not the same thing.
//
// The Exists helpers return false on a query error, which is the right
// direction where false means "not applied, re-run an idempotent statement".
// Written as !ColumnExists(...) to assert a DROP, that direction inverts: a
// query that could not run reports the post-condition as HOLDING, and the
// migration is recorded as applied without anything having checked. These
// return false on error, so a failed check re-runs a DROP ... IF EXISTS and
// costs nothing.
//
// Every VERIFY predicate in migrations.go now uses these; the sweep is done and
// TestMigrationVerifyPredicatesAssertAbsenceWithTheAbsenceHelpers keeps it that
// way. The `!ColumnExists` spelling that remains there is all inside migration
// BODIES, guarding whether to do the work, and its direction is the correct one
// for that job: on a failed check it attempts the idempotent DDL rather than
// skipping it. Swapping those would invert a check that is already right, which
// is why the lint keys on the parameter type instead of the spelling.
func ColumnAbsent(c Querier, table, column string) bool {
	var exists bool
	if err := c.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name=$1 AND column_name=$2)`,
		table, column,
	).Scan(&exists); err != nil {
		return false
	}
	return !exists
}

// TableAbsent is the absence form for a migration that DROPS a table. Same
// direction as ColumnAbsent: false on a query error, so a check that could not
// run re-runs an idempotent DROP ... IF EXISTS instead of recording the
// migration as applied.
func TableAbsent(c Querier, table string) bool {
	var exists bool
	if err := c.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name=$1)`,
		table,
	).Scan(&exists); err != nil {
		return false
	}
	return !exists
}

func IndexAbsent(c Querier, indexName string) bool {
	var exists bool
	if err := c.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname=$1)`, indexName,
	).Scan(&exists); err != nil {
		return false
	}
	return !exists
}

// NodePropertyKeyAbsent reports whether NO node carries the named property key.
//
// IT RETURNS FALSE ON A QUERY ERROR, and that direction is the whole reason it
// exists rather than being written inline. A verify predicate answers "does the
// post-condition hold" — for a migration that DELETES something, that is an
// absence, and an absence inferred from a query that did not run is the
// "absence of data rendering as absence of a problem" failure: the migration
// gets recorded as applied and never re-runs. False means "not verified", which
// re-runs an idempotent DELETE and costs nothing.
//
// The neighbouring Exists helpers can return false on error safely because they
// are used in the POSITIVE direction, where false already means "re-run". An
// absence assertion written as !ColumnExists(...) inverts that and inherits the
// wrong direction — see the note in migrations.go's v100.
func NodePropertyKeyAbsent(c Querier, key string) bool {
	var exists bool
	if err := c.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM node_properties WHERE key = $1)`, key,
	).Scan(&exists); err != nil {
		return false
	}
	return !exists
}

// IndexExists reports whether the named index exists in the database's
// public schema. Returns false on any query error or if the name is empty.
func IndexExists(c Querier, indexName string) bool {
	var exists bool
	c.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname=$1)`,
		indexName,
	).Scan(&exists)
	return exists
}

// ColumnType returns the SQL data type of the named column (e.g.
// "boolean", "integer", "text"), or an empty string if the column
// does not exist or any query error occurs.
func ColumnType(c Querier, table, column string) string {
	var dataType string
	c.QueryRow(
		`SELECT data_type FROM information_schema.columns WHERE table_name=$1 AND column_name=$2`,
		table, column,
	).Scan(&dataType)
	return dataType
}
