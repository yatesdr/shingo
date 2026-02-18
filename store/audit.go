package store

import (
	"time"
)

type AuditEntry struct {
	ID         int64
	EntityType string
	EntityID   int64
	Action     string
	OldValue   string
	NewValue   string
	Actor      string
	CreatedAt  time.Time
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
	var entries []*AuditEntry
	for rows.Next() {
		var e AuditEntry
		var createdAt string
		if err := rows.Scan(&e.ID, &e.EntityType, &e.EntityID, &e.Action, &e.OldValue, &e.NewValue, &e.Actor, &createdAt); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		entries = append(entries, &e)
	}
	return entries, rows.Err()
}

func (db *DB) ListEntityAudit(entityType string, entityID int64) ([]*AuditEntry, error) {
	rows, err := db.Query(db.Q(`SELECT id, entity_type, entity_id, action, old_value, new_value, actor, created_at FROM audit_log WHERE entity_type=? AND entity_id=? ORDER BY id DESC`), entityType, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []*AuditEntry
	for rows.Next() {
		var e AuditEntry
		var createdAt string
		if err := rows.Scan(&e.ID, &e.EntityType, &e.EntityID, &e.Action, &e.OldValue, &e.NewValue, &e.Actor, &createdAt); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		entries = append(entries, &e)
	}
	return entries, rows.Err()
}
