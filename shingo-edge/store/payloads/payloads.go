// Package payloads holds payload-catalog persistence for shingo-edge.
//
// Phase 5b of the architecture plan moved the payload_catalog CRUD out
// of the flat store/ package and into this sub-package. The outer
// store/ keeps a type alias (`store.PayloadCatalogEntry =
// payloads.CatalogEntry`) and one-line delegate methods on *store.DB so
// external callers see no API change.
package payloads

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"shingoedge/store/internal/helpers"
)

// CatalogEntry is one payload_catalog row.
type CatalogEntry struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Code        string    `json:"code"`
	Description string    `json:"description"`
	UOPCapacity int       `json:"uop_capacity"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func scanCatalogEntry(scanner interface{ Scan(...interface{}) error }) (*CatalogEntry, error) {
	e := &CatalogEntry{}
	var updatedAt string
	if err := scanner.Scan(&e.ID, &e.Name, &e.Code, &e.Description, &e.UOPCapacity, &updatedAt); err != nil {
		return nil, err
	}
	e.UpdatedAt = helpers.ScanTime(updatedAt)
	return e, nil
}

// UpsertCatalog inserts or updates a payload_catalog row.
func UpsertCatalog(db *sql.DB, entry *CatalogEntry) error {
	_, err := db.Exec(`INSERT INTO payload_catalog (id, name, code, description, uop_capacity, updated_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, code=excluded.code,
		description=excluded.description, uop_capacity=excluded.uop_capacity, updated_at=datetime('now')`,
		entry.ID, entry.Name, entry.Code, entry.Description, entry.UOPCapacity)
	return err
}

// ListCatalog returns every payload_catalog row sorted by name.
func ListCatalog(db *sql.DB) ([]*CatalogEntry, error) {
	rows, err := db.Query(`SELECT id, name, code, description, uop_capacity, updated_at FROM payload_catalog ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []*CatalogEntry
	for rows.Next() {
		e, err := scanCatalogEntry(rows)
		if err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// GetCatalogByCode returns a single payload_catalog row by code.
func GetCatalogByCode(db *sql.DB, code string) (*CatalogEntry, error) {
	return scanCatalogEntry(db.QueryRow(`SELECT id, name, code, description, uop_capacity, updated_at FROM payload_catalog WHERE code=?`, code))
}

// DeleteStaleCatalogEntries removes local catalog entries whose IDs are
// not in activeIDs. If activeIDs is empty, no entries are removed
// (safety: an empty list would delete all entries).
func DeleteStaleCatalogEntries(db *sql.DB, activeIDs []int64) error {
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
