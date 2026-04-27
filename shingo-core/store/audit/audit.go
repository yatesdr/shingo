// Package audit holds audit-log persistence for shingo-core.
//
// Phase 5 of the architecture plan moved audit_log CRUD out of the flat
// store/ package and into this sub-package. The outer store/ keeps a
// type alias (`store.AuditEntry = audit.Entry`) and one-line delegate
// methods on *store.DB so external callers see no API change.
package audit

import (
	"database/sql"

	"shingocore/domain"
)

// Entry is the audit-log row entity. The struct lives in
// shingocore/domain (Stage 2A.2); this alias keeps the audit.Entry
// name that every read helper, scan function, and Append call site
// in this package uses, and that www handlers + the audit service
// reference for response shapes.
type Entry = domain.AuditEntry

// Append writes an audit-log row.
func Append(db *sql.DB, entityType string, entityID int64, action, oldValue, newValue, actor string) error {
	_, err := db.Exec(`INSERT INTO audit_log (entity_type, entity_id, action, old_value, new_value, actor) VALUES ($1, $2, $3, $4, $5, $6)`,
		entityType, entityID, action, oldValue, newValue, actor)
	return err
}

// List returns the most recent audit-log entries up to limit.
func List(db *sql.DB, limit int) ([]*Entry, error) {
	rows, err := db.Query(`SELECT id, entity_type, entity_id, action, old_value, new_value, actor, created_at FROM audit_log ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntries(rows)
}

// ListForEntity returns all audit-log entries for one entity.
func ListForEntity(db *sql.DB, entityType string, entityID int64) ([]*Entry, error) {
	rows, err := db.Query(`SELECT id, entity_type, entity_id, action, old_value, new_value, actor, created_at FROM audit_log WHERE entity_type=$1 AND entity_id=$2 ORDER BY id DESC`, entityType, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntries(rows)
}

func scanEntries(rows *sql.Rows) ([]*Entry, error) {
	var entries []*Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.EntityType, &e.EntityID, &e.Action, &e.OldValue, &e.NewValue, &e.Actor, &e.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, &e)
	}
	return entries, rows.Err()
}
