package payloads

import "database/sql"

// insertID executes an INSERT ... RETURNING id query and returns the new row ID.
// Duplicated in each store sub-package to keep sub-packages zero-dependency.
func insertID(db *sql.DB, query string, args ...any) (int64, error) {
	var id int64
	err := db.QueryRow(query, args...).Scan(&id)
	return id, err
}
