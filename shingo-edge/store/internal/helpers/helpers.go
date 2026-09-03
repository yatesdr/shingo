// Package helpers holds shared low-level utilities used by every edge
// store sub-package. It lives under store/internal/ so its visibility
// is bounded to packages under shingoedge/store/... (Go's internal/
// rule).
//
// Mirrors the shingocore/store/internal/helpers pattern introduced in
// Phase 5a of the architecture plan. Edge-specific twist: the SQLite
// schema stores timestamps as strings ("2006-01-02 15:04:05"), so the
// helpers here include ScanTime / ScanTimePtr in addition to SQL
// parameter utilities. The TimeLayout constant is re-exported so
// sub-packages and top-level cross-aggregate code format timestamps the
// same way when they need to pass a computed cutoff back into SQLite.
package helpers

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// TimeLayout is the canonical SQLite timestamp format used by edge.
const TimeLayout = "2006-01-02 15:04:05"

// RowScanner is implemented by *sql.Rows and allows scan helper
// functions to iterate over query results generically without
// requiring callers to import database/sql just for the interface.
type RowScanner interface {
	Next() bool
	Scan(...any) error
	Err() error
}

// ScanTime parses a SQLite-formatted UTC timestamp. Returns zero time
// on parse error (matches the previous edge-store behaviour: scan
// helpers never fail on bad timestamp strings, they produce a zero
// time that the caller is expected to handle).
//
// IT ACCEPTS RFC3339 TOO, and that is a fix rather than generosity.
// TimeLayout is what most of this schema stores, but several columns are
// deliberately written as RFC3339Nano — demand_origins_open.opened_at,
// supply_refusals, style_node_claims.below_reorder_since — and each of
// those grew its own local time.Parse beside its own writer. The moment
// one such column is read through a GENERIC scan helper instead, the
// mismatch is silent: ParseInLocation fails, the zero time is returned,
// and the caller sees a column that is set in the database as unset in
// Go. That is exactly what happened to below_reorder_since, whose reader
// is scanNodeClaim: the falling edge was stamped on every tick and never
// once cleared, because the rising-edge branch only clears a flag it can
// see. Widening the parse costs one failed attempt on the uncommon
// layout and removes the whole failure mode.
func ScanTime(s string) time.Time {
	if t, err := time.ParseInLocation(TimeLayout, s, time.UTC); err == nil {
		return t
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t.UTC()
}

// ScanTimePtr parses an optional timestamp from a sql.NullString.
// Returns nil for a NULL or un-parseable timestamp. Accepts both layouts,
// for the reason on ScanTime.
func ScanTimePtr(ns sql.NullString) *time.Time {
	if !ns.Valid {
		return nil
	}
	if t, err := time.ParseInLocation(TimeLayout, ns.String, time.UTC); err == nil {
		return &t
	}
	t, err := time.Parse(time.RFC3339Nano, ns.String)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// SlugName converts a human name to a URL/code-safe slug. If the
// result is empty the fallback string is returned instead.
func SlugName(name, fallback string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return fallback
	}
	var b strings.Builder
	prevDash := false
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return fallback
	}
	return out
}

// GenerateUniqueCode finds a unique code within a scoped table. It
// tries `base` first, then `base-2`, `base-3`, etc. up to `base-9999`.
// table is the SQL table name, scopeCol is the column used to scope
// uniqueness (e.g. "process_id"), and scopeID is the value for that
// column. Takes an *sql.DB directly (not *store.DB) so this helper is
// reachable from every sub-package without creating an import cycle
// through the outer store/ package.
func GenerateUniqueCode(db *sql.DB, table, scopeCol string, scopeID int64, base, fallback string) (string, error) {
	if base == "" {
		base = fallback
	}
	query := fmt.Sprintf(`SELECT 1 FROM %s WHERE %s=? AND code=? LIMIT 1`, table, scopeCol)
	for i := 1; i <= 9999; i++ {
		candidate := base
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", base, i)
		}
		var exists int
		err := db.QueryRow(query, scopeID, candidate).Scan(&exists)
		if err == sql.ErrNoRows {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not generate unique code in %s", table)
}
