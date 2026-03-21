package store

import "time"

type RecoveryAction struct {
	ID         int64     `json:"id"`
	Action     string    `json:"action"`
	TargetType string    `json:"target_type"`
	TargetID   int64     `json:"target_id"`
	Detail     string    `json:"detail"`
	Actor      string    `json:"actor"`
	CreatedAt  time.Time `json:"created_at"`
}

func (db *DB) RecordRecoveryAction(action, targetType string, targetID int64, detail, actor string) error {
	_, err := db.Exec(`INSERT INTO recovery_actions (action, target_type, target_id, detail, actor) VALUES ($1, $2, $3, $4, $5)`,
		action, targetType, targetID, detail, actor)
	return err
}

func (db *DB) ListRecoveryActions(limit int) ([]*RecoveryAction, error) {
	rows, err := db.Query(`SELECT id, action, target_type, target_id, detail, actor, created_at FROM recovery_actions ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []*RecoveryAction
	for rows.Next() {
		var a RecoveryAction
		if err := rows.Scan(&a.ID, &a.Action, &a.TargetType, &a.TargetID, &a.Detail, &a.Actor, &a.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, &a)
	}
	return items, rows.Err()
}
