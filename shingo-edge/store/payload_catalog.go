package store

import (
	"log"
	"time"
)

type PayloadCatalogEntry struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Code        string    `json:"code"`
	Description string    `json:"description"`
	UOPCapacity int       `json:"uop_capacity"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (db *DB) UpsertPayloadCatalog(entry *PayloadCatalogEntry) error {
	_, err := db.Exec(`INSERT INTO payload_catalog (id, name, code, description, uop_capacity, updated_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, code=excluded.code,
		description=excluded.description, uop_capacity=excluded.uop_capacity, updated_at=datetime('now')`,
		entry.ID, entry.Name, entry.Code, entry.Description, entry.UOPCapacity)
	return err
}

func (db *DB) ListPayloadCatalog() ([]*PayloadCatalogEntry, error) {
	rows, err := db.Query(`SELECT id, name, code, description, uop_capacity, updated_at FROM payload_catalog ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*PayloadCatalogEntry
	for rows.Next() {
		e := &PayloadCatalogEntry{}
		if err := rows.Scan(&e.ID, &e.Name, &e.Code, &e.Description, &e.UOPCapacity, &e.UpdatedAt); err != nil {
			log.Printf("scan payload catalog row: %v", err)
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (db *DB) GetPayloadCatalogByCode(code string) (*PayloadCatalogEntry, error) {
	e := &PayloadCatalogEntry{}
	err := db.QueryRow(`SELECT id, name, code, description, uop_capacity, updated_at FROM payload_catalog WHERE code=?`, code).
		Scan(&e.ID, &e.Name, &e.Code, &e.Description, &e.UOPCapacity, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return e, nil
}

