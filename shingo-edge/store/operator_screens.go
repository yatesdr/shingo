package store

import (
	"encoding/json"
	"time"
)

// OperatorScreen represents a saved operator display layout.
type OperatorScreen struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Slug      string          `json:"slug"`
	Layout    json.RawMessage `json:"layout"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func (db *DB) ListOperatorScreens() ([]OperatorScreen, error) {
	rows, err := db.Query(`SELECT id, name, slug, layout, created_at, updated_at FROM operator_screens ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var screens []OperatorScreen
	for rows.Next() {
		var s OperatorScreen
		var layout, createdAt, updatedAt string
		if err := rows.Scan(&s.ID, &s.Name, &s.Slug, &layout, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		s.Layout = json.RawMessage(layout)
		s.CreatedAt = scanTime(createdAt)
		s.UpdatedAt = scanTime(updatedAt)
		screens = append(screens, s)
	}
	return screens, rows.Err()
}

func (db *DB) GetOperatorScreen(id int64) (*OperatorScreen, error) {
	s := &OperatorScreen{}
	var layout, createdAt, updatedAt string
	err := db.QueryRow(`SELECT id, name, slug, layout, created_at, updated_at FROM operator_screens WHERE id = ?`, id).
		Scan(&s.ID, &s.Name, &s.Slug, &layout, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	s.Layout = json.RawMessage(layout)
	s.CreatedAt = scanTime(createdAt)
	s.UpdatedAt = scanTime(updatedAt)
	return s, nil
}

func (db *DB) CreateOperatorScreen(name, slug string, layout json.RawMessage) (int64, error) {
	res, err := db.Exec(`INSERT INTO operator_screens (name, slug, layout) VALUES (?, ?, ?)`, name, slug, string(layout))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateOperatorScreenLayout(id int64, layout json.RawMessage) error {
	_, err := db.Exec(`UPDATE operator_screens SET layout=?, updated_at=datetime('now') WHERE id=?`, string(layout), id)
	return err
}
