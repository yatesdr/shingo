package nodes

import (
	"database/sql"
)

// nullableInt converts a *int to a value suitable for SQL params (nil-safe).
func nullableInt(p *int) any {
	if p != nil {
		return *p
	}
	return nil
}

// nullableInt64 converts a *int64 to a value suitable for SQL params (nil-safe).
func nullableInt64(p *int64) any {
	if p != nil {
		return *p
	}
	return nil
}

// insertID executes an INSERT ... RETURNING id query and returns the new row ID.
// Duplicated per sub-package so aggregates stay zero-dependency.
func insertID(db *sql.DB, query string, args ...any) (int64, error) {
	var id int64
	err := db.QueryRow(query, args...).Scan(&id)
	return id, err
}
