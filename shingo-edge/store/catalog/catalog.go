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
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	Code         string  `json:"code"`
	Description  string  `json:"description"`
	UOPCapacity  int     `json:"uop_capacity"`
	CycleSeconds float64 `json:"cycle_seconds"`
	// CATID is the payload's part identity, synced from Core (its single distinct
	// manifest part number, or empty when the payload maps to zero or several).
	// The claim editor auto-fills a style's expected_catid from this.
	CATID     string    `json:"catid"`
	UpdatedAt time.Time `json:"updated_at"`
}

const catalogSelectCols = `id, name, code, description, uop_capacity, cycle_seconds, COALESCE(catid, '') as catid, updated_at`

func scanCatalogEntry(scanner interface{ Scan(...any) error }) (*CatalogEntry, error) {
	e := &CatalogEntry{}
	var updatedAt string
	if err := scanner.Scan(&e.ID, &e.Name, &e.Code, &e.Description, &e.UOPCapacity, &e.CycleSeconds, &e.CATID, &updatedAt); err != nil {
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
//
// The upsert is CONDITIONAL: an unchanged row is not written (and its
// updated_at is not stamped). updated_at means "last changed", not "last
// synced" — nothing reads it as liveness (verified 2026-08-19: no JS,
// template, or Go reader consumes the field beyond serialization).
func UpsertCatalog(db *sql.DB, entry *CatalogEntry) error {
	_, err := db.Exec(`INSERT INTO payload_catalog (id, name, code, description, uop_capacity, catid, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, code=excluded.code,
		description=excluded.description, uop_capacity=excluded.uop_capacity,
		catid=excluded.catid, updated_at=datetime('now')
		WHERE payload_catalog.name        IS NOT excluded.name
		   OR payload_catalog.code        IS NOT excluded.code
		   OR payload_catalog.description IS NOT excluded.description
		   OR payload_catalog.uop_capacity IS NOT excluded.uop_capacity
		   OR payload_catalog.catid       IS NOT excluded.catid`,
		entry.ID, entry.Name, entry.Code, entry.Description, entry.UOPCapacity, entry.CATID)
	return err
}

// SyncCatalog upserts the full Core catalog and prunes stale entries in ONE
// transaction. The 2-minute sync used to run 57 separate implicit
// transactions (115–326 ms holds of the edge's single SQLite connection, one
// observed 2,450 ms — ~41,000 write txns/day) just to write back rows that
// almost never change; one tx + the conditional upsert above turns that into
// one short write txn per sync that only fires when something actually
// changed. On any error the tx rolls back: the previous last-known-good
// catalog stands, same doctrine as ReplaceCoreLoaders.
func SyncCatalog(db *sql.DB, entries []*CatalogEntry) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	ids := make([]int64, 0, len(entries))
	for _, entry := range entries {
		if _, err := tx.Exec(`INSERT INTO payload_catalog (id, name, code, description, uop_capacity, catid, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, datetime('now'))
			ON CONFLICT(id) DO UPDATE SET name=excluded.name, code=excluded.code,
			description=excluded.description, uop_capacity=excluded.uop_capacity,
			catid=excluded.catid, updated_at=datetime('now')
			WHERE payload_catalog.name        IS NOT excluded.name
			   OR payload_catalog.code        IS NOT excluded.code
			   OR payload_catalog.description IS NOT excluded.description
			   OR payload_catalog.uop_capacity IS NOT excluded.uop_capacity
			   OR payload_catalog.catid       IS NOT excluded.catid`,
			entry.ID, entry.Name, entry.Code, entry.Description, entry.UOPCapacity, entry.CATID); err != nil {
			return fmt.Errorf("upsert catalog entry %s: %w", entry.Name, err)
		}
		ids = append(ids, entry.ID)
	}
	// The stale-entry prune joins the same tx — atomic with the upserts, so a
	// catalog that pruned-but-didn't-refresh (or vice versa) can't exist.
	if err := deleteStaleCatalogEntriesTx(tx, ids); err != nil {
		return err
	}
	return tx.Commit()
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
			return entries, fmt.Errorf("scan catalog row: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
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
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteStaleCatalogEntriesTx(tx, activeIDs); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteStaleCatalogEntriesTx is the prune shared by the standalone call and
// SyncCatalog's single transaction.
func deleteStaleCatalogEntriesTx(tx *sql.Tx, activeIDs []int64) error {
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
	_, err := tx.Exec(query, args...)
	return err
}
