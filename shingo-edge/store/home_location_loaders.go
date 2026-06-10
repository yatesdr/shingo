package store

// home_location_loaders — Edge-only membership set marking a bin loader as
// "home location" layout: each payload gets its own dedicated physical position
// (one manual_swap node per payload), rather than the default "single window"
// layout where many payloads share one slot. Mirrors transitional_loaders.
//
//   - Keyed by core_node_name. Each home position is its own core node; a row
//     marks that node as a home in a home-location loader. A row absent = single
//     window (the default — today's behaviour, one slot many payloads).
//   - Membership = home-location layout. The operator board renders a member's
//     station as one card per home (position × its payload); a non-member single
//     loader renders as one window with a card per payload.
//   - Edge-only, a display/layout concern. Never plumbed through ClaimSync.
//   - ORTHOGONAL to transitional_loaders: a loader independently picks a TYPE
//     (traditional / transitional) and a LAYOUT (single-window / home-location).
//     The two sets never need to agree.

import "database/sql"

// IsHomeLocationLoader reports whether a home_location_loaders row exists for
// coreNodeName (i.e. this loader node uses the dedicated home-location layout).
func (db *DB) IsHomeLocationLoader(coreNodeName string) (bool, error) {
	var one int
	err := db.QueryRow(`SELECT 1 FROM home_location_loaders WHERE core_node_name = ?`, coreNodeName).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListHomeLocationLoaders returns the core_node_name of every home-location
// loader node, sorted. Used by the admin UI and the station view-model builder.
func (db *DB) ListHomeLocationLoaders() ([]string, error) {
	rows, err := db.Query(`SELECT core_node_name FROM home_location_loaders ORDER BY core_node_name`)
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

// SetHomeLocationLoader adds (homeLocation=true, idempotent) or removes (false)
// coreNodeName from the set. updatedBy is recorded for the audit column on
// insert.
func (db *DB) SetHomeLocationLoader(coreNodeName string, homeLocation bool, updatedBy string) error {
	if homeLocation {
		_, err := db.Exec(`
			INSERT INTO home_location_loaders (core_node_name, updated_at, updated_by)
			VALUES (?, datetime('now'), ?)
			ON CONFLICT(core_node_name) DO UPDATE SET
				updated_at = datetime('now'),
				updated_by = excluded.updated_by`,
			coreNodeName, updatedBy)
		return err
	}
	_, err := db.Exec(`DELETE FROM home_location_loaders WHERE core_node_name = ?`, coreNodeName)
	return err
}
