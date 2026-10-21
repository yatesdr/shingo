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
// keeps one-line delegate methods on *store.DB; callers name
// catalog.CatalogEntry directly.
package catalog

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"shingo/protocol"
	"shingoedge/store/internal/helpers"
)

// CatalogEntry is one payload_catalog row.
type CatalogEntry struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Code        string `json:"code"`
	Description string `json:"description"`
	UOPCapacity int    `json:"uop_capacity"`
	// CATID is the payload's part identity, synced from Core: the DISTINCT
	// manifest part numbers comma-joined (a single value for a one-part
	// payload, the full list for a multi-part kit), empty when the manifest
	// carries no part numbers. The engine splits it into the style's
	// part-identity set — the same comma convention as a manual
	// expected_catid pin.
	CATID string `json:"catid"`
	// Manifest is the payload's manifest template lines in Core's order, as
	// the catalog feed carries them; nil when the payload has none. Stored as
	// JSON in payload_catalog.manifest (Edge v28). The load modal's manifest
	// read (www apiPayloadManifest) answers from it.
	Manifest  []protocol.ManifestItem `json:"manifest"`
	UpdatedAt time.Time               `json:"updated_at"`
}

const catalogSelectCols = `id, name, code, description, uop_capacity, COALESCE(catid, '') as catid, manifest, updated_at`

func scanCatalogEntry(scanner interface{ Scan(...any) error }) (*CatalogEntry, error) {
	e := &CatalogEntry{}
	var updatedAt, manifest string
	if err := scanner.Scan(&e.ID, &e.Name, &e.Code, &e.Description, &e.UOPCapacity, &e.CATID, &manifest, &updatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(manifest), &e.Manifest); err != nil {
		return nil, fmt.Errorf("catalog %s manifest: %w", e.Code, err)
	}
	if len(e.Manifest) == 0 {
		e.Manifest = nil
	}
	e.UpdatedAt = helpers.ScanTime(updatedAt)
	return e, nil
}

// manifestColumn is the JSON a row's manifest is stored as: "[]" for none,
// so an unchanged row with no lines compares equal in the conditional upsert.
func manifestColumn(lines []protocol.ManifestItem) (string, error) {
	if len(lines) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(lines)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// upsertCatalogSQL is the conditional upsert both writes use: an unchanged
// row, its lines included, is not written.
const upsertCatalogSQL = `INSERT INTO payload_catalog (id, name, code, description, uop_capacity, catid, manifest, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, code=excluded.code,
		description=excluded.description, uop_capacity=excluded.uop_capacity,
		catid=excluded.catid, manifest=excluded.manifest, updated_at=datetime('now')
		WHERE payload_catalog.name        IS NOT excluded.name
		   OR payload_catalog.code        IS NOT excluded.code
		   OR payload_catalog.description IS NOT excluded.description
		   OR payload_catalog.uop_capacity IS NOT excluded.uop_capacity
		   OR payload_catalog.catid       IS NOT excluded.catid
		   OR payload_catalog.manifest    IS NOT excluded.manifest`

// UpsertCatalog inserts or updates a payload_catalog row from a Core
// sync payload.
//
// The upsert is CONDITIONAL: an unchanged row is not written (and its
// updated_at is not stamped). updated_at means "last changed", not "last
// synced" — nothing reads it as liveness (verified 2026-08-19: no JS,
// template, or Go reader consumes the field beyond serialization).
func UpsertCatalog(db *sql.DB, entry *CatalogEntry) error {
	manifest, err := manifestColumn(entry.Manifest)
	if err != nil {
		return fmt.Errorf("encode catalog %s manifest: %w", entry.Code, err)
	}
	_, err = db.Exec(upsertCatalogSQL,
		entry.ID, entry.Name, entry.Code, entry.Description, entry.UOPCapacity, entry.CATID, manifest)
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
		manifest, err := manifestColumn(entry.Manifest)
		if err != nil {
			return fmt.Errorf("encode catalog %s manifest: %w", entry.Code, err)
		}
		if _, err := tx.Exec(upsertCatalogSQL,
			entry.ID, entry.Name, entry.Code, entry.Description, entry.UOPCapacity, entry.CATID, manifest); err != nil {
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
