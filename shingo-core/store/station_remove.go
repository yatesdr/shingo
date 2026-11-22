package store

import (
	"fmt"
	"strings"

	"shingocore/store/audit"
	"shingocore/store/plantclaims"
	"shingocore/store/registry"
)

// RemoveStaleStation removes a stale station in one transaction, audited
// (owner ruling 2026-11-22, Remove station): its registry row, and its claims
// mirror with the report rows Core quotes of it (plantclaims.RemoveStation).
// NOTHING ELSE: its history, orders, bins, counts, demand registry rows, node
// assignments, cells and the loaders that name it are left as they are. It
// returns the processes whose mirror it cleared. A station that is not stale
// is registry.ErrNotStale, and nothing is written. Six statements.
func (db *DB) RemoveStaleStation(uid, actor string) ([]string, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("remove station %s: begin: %w", uid, err)
	}
	defer func() { _ = tx.Rollback() }()
	id, name, err := registry.RemoveStale(tx, uid)
	if err != nil {
		return nil, err
	}
	processes, err := plantclaims.RemoveStation(tx, uid)
	if err != nil {
		return nil, err
	}
	was := fmt.Sprintf("station %s %q, claims of [%s]", uid, name, strings.Join(processes, ", "))
	if err := audit.Append(tx, "edge", id, "removed", was, "", actor); err != nil {
		return nil, fmt.Errorf("remove station %s: audit: %w", uid, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("remove station %s: commit: %w", uid, err)
	}
	return processes, nil
}
