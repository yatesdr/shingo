package store

// operator_driven_loaders — Edge-only membership set marking a bin loader as
// operator-driven (the operator stages/clears at the board rather than the
// automatic replenishment path supplying it). Renamed from the old
// "transitional_loaders" — same set, clearer name.
//
//   - Keyed by core_node_name alone. An operator-driven loader is operator-
//     driven as a whole, and core_node_name is the only granularity that is
//     1:1 with the physical loader: a loader shared across processes/styles
//     (e.g. SNF2 + SNF3 feeding one loader) has multiple style_node_claims
//     and process_nodes rows but one core node. A per-claim bool would have
//     no defined reduction when two styles disagree; this set avoids that.
//   - Membership = operator-driven. A row present suppresses the market-
//     accounting L1 paths (UOP-threshold C-push and legacy bin-count) for
//     the loader; deleting it returns the loader to those automatic paths.
//   - Edge-only. Never plumbed through ClaimSync — Core's threshold monitor
//     already idles for these loaders because their thresholds are 0.

import "database/sql"

// IsOperatorDrivenLoader reports whether an operator_driven_loaders row exists
// for coreNodeName.
func (db *DB) IsOperatorDrivenLoader(coreNodeName string) (bool, error) {
	var one int
	err := db.QueryRow(`SELECT 1 FROM operator_driven_loaders WHERE core_node_name = ?`, coreNodeName).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListOperatorDrivenLoaders returns the core_node_name of every operator-driven
// loader, sorted. Used by the admin UI and the station view-model builder.
func (db *DB) ListOperatorDrivenLoaders() ([]string, error) {
	rows, err := db.Query(`SELECT core_node_name FROM operator_driven_loaders ORDER BY core_node_name`)
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

// SetOperatorDrivenLoader adds (operatorDriven=true, idempotent) or removes
// (false) coreNodeName from the set. updatedBy is recorded for the audit
// column on insert.
func (db *DB) SetOperatorDrivenLoader(coreNodeName string, operatorDriven bool, updatedBy string) error {
	if operatorDriven {
		_, err := db.Exec(`
			INSERT INTO operator_driven_loaders (core_node_name, updated_at, updated_by)
			VALUES (?, datetime('now'), ?)
			ON CONFLICT(core_node_name) DO UPDATE SET
				updated_at = datetime('now'),
				updated_by = excluded.updated_by`,
			coreNodeName, updatedBy)
		return err
	}
	_, err := db.Exec(`DELETE FROM operator_driven_loaders WHERE core_node_name = ?`, coreNodeName)
	return err
}
