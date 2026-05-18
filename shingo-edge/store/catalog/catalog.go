// Package catalog holds payload-catalog persistence for shingo-edge.
// The catalog is synced from core's `payloads/` (which holds the
// canonical payload templates); edge keeps a local read-through copy
// so HMI lookups don't have to hit core for every render.
//
// Phase 5b moved this CRUD out of the flat store/ package; Phase 6.0c
// renamed the sub-package from `payloads/` to `catalog/`. The rename
// disambiguates from core's `payloads/` (which holds the source-of-
// truth template definitions) — same word, different responsibility.
// On-disk table name `payload_catalog` is unchanged. The outer store/
// keeps a type alias (`store.PayloadCatalogEntry = catalog.CatalogEntry`)
// and one-line delegate methods on *store.DB so external callers see
// no API change.
package catalog

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"shingoedge/store/internal/helpers"
)

// CatalogEntry is one payload_catalog row.
//
// CycleSeconds is Edge-local: not synced from Core, engineer-edited via
// the replenishment page, preserved across UpsertCatalog calls so the
// catalog sync (which only refreshes the synced columns) doesn't wipe it.
type CatalogEntry struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Code         string    `json:"code"`
	Description  string    `json:"description"`
	UOPCapacity  int       `json:"uop_capacity"`
	CycleSeconds float64   `json:"cycle_seconds"`
	UpdatedAt    time.Time `json:"updated_at"`
}

const catalogSelectCols = `id, name, code, description, uop_capacity, cycle_seconds, updated_at`

func scanCatalogEntry(scanner interface{ Scan(...interface{}) error }) (*CatalogEntry, error) {
	e := &CatalogEntry{}
	var updatedAt string
	if err := scanner.Scan(&e.ID, &e.Name, &e.Code, &e.Description, &e.UOPCapacity, &e.CycleSeconds, &updatedAt); err != nil {
		return nil, err
	}
	e.UpdatedAt = helpers.ScanTime(updatedAt)
	return e, nil
}

// UpsertCatalog inserts or updates a payload_catalog row from a Core
// sync payload. cycle_seconds is deliberately excluded from the
// ON CONFLICT update list so the engineer-edited Edge-local value is
// preserved across syncs. On INSERT the column takes its DEFAULT 0;
// SetCycleSeconds is the engineer-edit path.
func UpsertCatalog(db *sql.DB, entry *CatalogEntry) error {
	_, err := db.Exec(`INSERT INTO payload_catalog (id, name, code, description, uop_capacity, updated_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, code=excluded.code,
		description=excluded.description, uop_capacity=excluded.uop_capacity, updated_at=datetime('now')`,
		entry.ID, entry.Name, entry.Code, entry.Description, entry.UOPCapacity)
	return err
}

// SetCycleSeconds writes the engineer-edited per-part cycle time. No-op
// (no error) if no row matches the code; the replenishment UI never
// surfaces parts the catalog doesn't already know about, so a missing
// row at this point is a sync race the caller can ignore.
func SetCycleSeconds(db *sql.DB, code string, seconds float64) error {
	_, err := db.Exec(`UPDATE payload_catalog SET cycle_seconds=?, updated_at=datetime('now') WHERE code=?`, seconds, code)
	return err
}

// ListCatalog returns every payload_catalog row sorted by name.
func ListCatalog(db *sql.DB) ([]*CatalogEntry, error) {
	rows, err := db.Query(`SELECT ` + catalogSelectCols + ` FROM payload_catalog ORDER BY name`)
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
	return scanCatalogEntry(db.QueryRow(`SELECT `+catalogSelectCols+` FROM payload_catalog WHERE code=?`, code))
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
