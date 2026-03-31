package store

import (
	"fmt"
	"strings"
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

func scanPayloadCatalogEntry(scanner interface{ Scan(...interface{}) error }) (*PayloadCatalogEntry, error) {
	e := &PayloadCatalogEntry{}
	var updatedAt string
	if err := scanner.Scan(&e.ID, &e.Name, &e.Code, &e.Description, &e.UOPCapacity, &updatedAt); err != nil {
		return nil, err
	}
	e.UpdatedAt = scanTime(updatedAt)
	return e, nil
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
		e, err := scanPayloadCatalogEntry(rows)
		if err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (db *DB) GetPayloadCatalogByCode(code string) (*PayloadCatalogEntry, error) {
	return scanPayloadCatalogEntry(db.QueryRow(`SELECT id, name, code, description, uop_capacity, updated_at FROM payload_catalog WHERE code=?`, code))
}

// DeleteStalePayloadCatalogEntries removes local catalog entries whose IDs are not in activeIDs.
// If activeIDs is empty, no entries are removed (safety: an empty list would delete all entries).
func (db *DB) DeleteStalePayloadCatalogEntries(activeIDs []int64) error {
	if len(activeIDs) == 0 {
		return nil
	}
	placeholders := make([]string, len(activeIDs))
	args := make([]any, len(activeIDs))
	for i, id := range activeIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf("DELETE FROM payload_catalog WHERE id NOT IN (%s)", strings.Join(placeholders, ","))
	_, err := db.Exec(query, args...)
	return err
}
