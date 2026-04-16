package store

import "time"

// nullableInt64 converts a *int64 to a value suitable for SQL params (nil-safe).
func nullableInt64(p *int64) any {
	if p != nil {
		return *p
	}
	return nil
}

// nullableTime converts a *time.Time to a UTC value suitable for SQL params (nil-safe).
func nullableTime(p *time.Time) any {
	if p != nil {
		return p.UTC()
	}
	return nil
}

// insertID executes an INSERT ... RETURNING id query and returns the new row ID.
func (db *DB) insertID(query string, args ...any) (int64, error) {
	var id int64
	err := db.QueryRow(query, args...).Scan(&id)
	return id, err
}
