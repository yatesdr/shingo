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
	_, err := db.Exec(`INSERT INTO audit_log (entity_type, entity_id, action, old_value, new_value, actor) VALUES ($1, $2, $3, $4, $5, $6)`,
		entityType, entityID, action, oldValue, newValue, actor)
	return err
}

func (db *DB) ListAuditLog(limit int) ([]*AuditEntry, error) {
	rows, err := db.Query(`SELECT id, entity_type, entity_id, action, old_value, new_value, actor, created_at FROM audit_log ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAuditEntries(rows)
}

func (db *DB) ListEntityAudit(entityType string, entityID int64) ([]*AuditEntry, error) {
	rows, err := db.Query(`SELECT id, entity_type, entity_id, action, old_value, new_value, actor, created_at FROM audit_log WHERE entity_type=$1 AND entity_id=$2 ORDER BY id DESC`, entityType, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAuditEntries(rows)
}

// AddBinNote appends a typed note to a bin's audit trail.
func (db *DB) AddBinNote(binID int64, noteType, message, actor string) error {
	return db.AppendAudit("bin", binID, "note:"+noteType, "", message, actor)
}

func scanAuditEntries(rows *sql.Rows) ([]*AuditEntry, error) {
	var entries []*AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.EntityType, &e.EntityID, &e.Action, &e.OldValue, &e.NewValue, &e.Actor, &e.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, &e)
	}
	return entries, rows.Err()
}
