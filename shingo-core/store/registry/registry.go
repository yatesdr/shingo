// Package registry holds edge-station registry persistence for
// shingo-core.
//
// Phase 5 of the architecture plan moved the edge_registry CRUD +
// heartbeat upsert + stale-edge sweep out of the flat store/ package
// and into this sub-package. The outer store/ keeps a type alias
// (`store.EdgeRegistration = registry.Edge`) and one-line delegate
// methods on *store.DB so external callers see no API change.
package registry

import (
	"database/sql"
	"encoding/json"
	"time"

	"shingocore/domain"
)

// Edge represents one registered edge station. The struct lives in
// shingocore/domain (Stage 2A.2); this alias keeps the registry.Edge
// name used by every read helper, scan function, and Register /
// MarkStale call site in this package, plus the outer store/
// re-export and the page-data builder that surfaces registry status
// to the admin UI.
type Edge = domain.RegistryEdge

// Register upserts an edge registration. If the station_id already
// exists, it updates the record and resets status to active.
func Register(db *sql.DB, stationID, hostname, version string, lineIDs []string) error {
	lineJSON, _ := json.Marshal(lineIDs)

	_, err := db.Exec(`
		INSERT INTO edge_registry (station_id, hostname, version, line_ids, registered_at, status)
		VALUES ($1, $2, $3, $4, NOW(), 'active')
		ON CONFLICT(station_id) DO UPDATE SET
			hostname = excluded.hostname,
			version = excluded.version,
			line_ids = excluded.line_ids,
			registered_at = excluded.registered_at,
			status = 'active'
	`, stationID, hostname, version, string(lineJSON))
	return err
}

// UpdateHeartbeat upserts last_heartbeat and sets status to active. If
// the edge hasn't registered yet, creates a minimal registry entry.
// Returns true if a new row was inserted (unregistered edge detected).
func UpdateHeartbeat(db *sql.DB, stationID string) (isNew bool, err error) {
	var exists bool
	db.QueryRow(`SELECT 1 FROM edge_registry WHERE station_id = $1`, stationID).Scan(&exists)

	_, err = db.Exec(`
		INSERT INTO edge_registry (station_id, last_heartbeat, status)
		VALUES ($1, NOW(), 'active')
		ON CONFLICT(station_id) DO UPDATE SET
			last_heartbeat = NOW(),
			status = 'active'
	`, stationID)
	return !exists, err
}

// List returns all registered edges.
func List(db *sql.DB) ([]Edge, error) {
	rows, err := db.Query(`
		SELECT id, station_id, hostname, version, line_ids, registered_at, last_heartbeat, status
		FROM edge_registry ORDER BY station_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []Edge
	for rows.Next() {
		var e Edge
		var lineJSON string
		if err := rows.Scan(&e.ID, &e.StationID, &e.Hostname, &e.Version, &lineJSON, &e.RegisteredAt, &e.LastHeartbeat, &e.Status); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(lineJSON), &e.LineIDs)
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// MarkStale sets status='stale' for active edges whose last_heartbeat is
// older than the threshold. Returns the marked station IDs.
func MarkStale(db *sql.DB, threshold time.Duration) ([]string, error) {
	cutoff := time.Now().UTC().Add(-threshold)

	rows, err := db.Query(`
		UPDATE edge_registry
		SET status = 'stale'
		WHERE status = 'active'
		  AND last_heartbeat IS NOT NULL
		  AND last_heartbeat < $1
		RETURNING station_id
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var staleIDs []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		staleIDs = append(staleIDs, sid)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(staleIDs) == 0 {
		return nil, nil
	}
	return staleIDs, nil
}
