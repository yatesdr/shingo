package store

import (
	"database/sql"
	"time"
)

type AuditEntry struct {
	ID         int64     `json:"id"`
	EntityType string    `json:"entity_type"`
	EntityID   int64     `json:"entity_id"`
	Action     string    `json:"action"`
	OldValue   string    `json:"old_value"`
	NewValue   string    `json:"new_value"`
	Actor      string    `json:"actor"`
	CreatedAt  time.Time `json:"created_at"`
}

func (db *DB) AppendAudit(entityType string, entityID int64, action, oldValue, newValue, actor string) error {
	_, err := db.Exec(db.Q(`INSERT INTO audit_log (entity_type, entity_id, action, old_value, new_value, actor) VALUES (?, ?, ?, ?, ?, ?)`),
		entityType, entityID, action, oldValue, newValue, actor)
	return err
}

func (db *DB) ListAuditLog(limit int) ([]*AuditEntry, error) {
	rows, err := db.Query(db.Q(`SELECT id, entity_type, entity_id, action, old_value, new_value, actor, created_at FROM audit_log ORDER BY id DESC LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAuditEntries(rows)
}

func (db *DB) ListEntityAudit(entityType string, entityID int64) ([]*AuditEntry, error) {
	rows, err := db.Query(db.Q(`SELECT id, entity_type, entity_id, action, old_value, new_value, actor, created_at FROM audit_log WHERE entity_type=? AND entity_id=? ORDER BY id DESC`), entityType, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAuditEntries(rows)
}

func scanAuditEntries(rows *sql.Rows) ([]*AuditEntry, error) {
	var entries []*AuditEntry
	for rows.Next() {
		var e AuditEntry
		var createdAt any
		if err := rows.Scan(&e.ID, &e.EntityType, &e.EntityID, &e.Action, &e.OldValue, &e.NewValue, &e.Actor, &createdAt); err != nil {
			return nil, err
		}
		e.CreatedAt = parseTime(createdAt)
		entries = append(entries, &e)
	}
	return entries, rows.Err()
}
