// Package helpers holds shared low-level utilities used by every store
// sub-package. It lives under store/internal/ so its visibility is bounded
// to packages under shingocore/store/... — engine, service, www, and other
// out-of-store callers cannot import it (Go's internal/ rule).
//
// The duplication this package eliminates (each sub-package previously
// carried its own helpers.go) was a deliberate Phase-pre-5 trade-off:
// keep aggregates zero-dependency until enough sub-packages exist to
// justify a shared internal package. Phase 5 crosses that threshold by
// adding 13 more core sub-packages.
package helpers

import (
	"database/sql"
	"time"
)

// NullableInt converts *int to a value safe for SQL params (nil-safe).
func NullableInt(p *int) any {
	if p != nil {
		return *p
	}
	return nil
}

// NullableInt64 converts *int64 to a value safe for SQL params (nil-safe).
func NullableInt64(p *int64) any {
	if p != nil {
		return *p
	}
	return nil
}

// NullableTime converts *time.Time to a UTC value safe for SQL params (nil-safe).
func NullableTime(p *time.Time) any {
	if p != nil {
		return p.UTC()
	}
	return nil
}

// InsertID executes an INSERT ... RETURNING id query and returns the new row ID.
func InsertID(db *sql.DB, query string, args ...any) (int64, error) {
	var id int64
	err := db.QueryRow(query, args...).Scan(&id)
	return id, err
}
