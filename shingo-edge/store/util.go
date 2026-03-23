package store

import (
	"database/sql"
	"fmt"
	"strings"
	"unicode"
)

// slugName converts a human name to a URL/code-safe slug. If the result
// is empty the fallback string is returned instead.
func slugName(name, fallback string) string {
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

// generateUniqueCode finds a unique code within a scoped table. It tries
// `base` first, then `base-2`, `base-3`, etc. up to `base-9999`.
// table is the SQL table name, scopeCol is the column used to scope
// uniqueness (e.g. "process_id"), and scopeID is the value for that column.
func generateUniqueCode(db *DB, table, scopeCol string, scopeID int64, base, fallback string) (string, error) {
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
