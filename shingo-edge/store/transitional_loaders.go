package store

// transitional_loaders — Edge-only membership set marking a bin loader as
// operator-driven (transitional). See
// transitional-bin-loader-plan-v2.md for the design overview.
//
//   - Keyed by core_node_name alone. A transitional loader is operator-
//     driven as a whole, and core_node_name is the only granularity that is
//     1:1 with the physical loader: a loader shared across processes/styles
//     (e.g. SNF2 + SNF3 feeding one loader) has multiple style_node_claims
//     and process_nodes rows but one core node. A per-claim bool would have
//     no defined reduction when two styles disagree; this set avoids that.
//   - Membership = transitional. A row present suppresses the market-
//     accounting L1 paths (UOP-threshold C-push and legacy bin-count) for
//     the loader; deleting it returns the loader to those automatic paths.
//   - Edge-only. Never plumbed through ClaimSync — Core's threshold monitor
//     already idles for these loaders because their thresholds are 0.

import "database/sql"

// IsTransitionalLoader reports whether a transitional_loaders row exists for
// coreNodeName.
func (db *DB) IsTransitionalLoader(coreNodeName string) (bool, error) {
	var one int
	err := db.QueryRow(`SELECT 1 FROM transitional_loaders WHERE core_node_name = ?`, coreNodeName).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListTransitionalLoaders returns the core_node_name of every transitional
// loader, sorted. Used by the admin UI and the station view-model builder.
func (db *DB) ListTransitionalLoaders() ([]string, error) {
	rows, err := db.Query(`SELECT core_node_name FROM transitional_loaders ORDER BY core_node_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SetTransitionalLoader adds (transitional=true, idempotent) or removes
// (false) coreNodeName from the set. updatedBy is recorded for the audit
// column on insert.
func (db *DB) SetTransitionalLoader(coreNodeName string, transitional bool, updatedBy string) error {
	if transitional {
		_, err := db.Exec(`
			INSERT INTO transitional_loaders (core_node_name, updated_at, updated_by)
			VALUES (?, datetime('now'), ?)
			ON CONFLICT(core_node_name) DO UPDATE SET
				updated_at = datetime('now'),
				updated_by = excluded.updated_by`,
			coreNodeName, updatedBy)
		return err
	}
	_, err := db.Exec(`DELETE FROM transitional_loaders WHERE core_node_name = ?`, coreNodeName)
	return err
}
