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
	"strconv"
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
//
// THE CUTOFF IS COMPUTED BY POSTGRES. That is what `NOW() - $1::interval`
// is for, and it is the whole content of this change.
//
// UpdateHeartbeat writes `last_heartbeat = NOW()`, so the write side has
// always been on the database clock. Deriving the cutoff from time.Now()
// on the caller's host — which is what this did — compared two
// independent clocks and silently added the difference to the caller's
// threshold: an edge went stale after `threshold + (db_clock - host_clock)`
// rather than after `threshold`.
//
// That term is neither small nor bounded. Core's Postgres runs on a
// separate server at both plants, nothing disciplines the two clocks
// against each other, and the error has a direction on each side:
//
//   - database AHEAD of Core's host: edges go stale LATE, by the skew.
//     Detection is delayed, and the delay is invisible.
//   - database BEHIND Core's host: edges go stale EARLY. As the skew
//     approaches the configured threshold the effective threshold reaches
//     zero, and every edge — including one heartbeating normally — is
//     marked stale on every 60-second tick, which also reaps that
//     station's demand_registry rows in CoreHandler.staleEdgeLoop.
//
// The same comparison is what made TestCoverage_MarkStaleEdges (I6)
// flake, where the two clocks are the test container's and the host's and
// the entire margin was a 5 ms sleep. Verified by injecting a 50 ms offset
// into the old host-side cutoff: the test failed deterministically with
// `stale ids len = 0, want 2`. NEITHER the threshold's value NOR
// t.Parallel() was implicated — any threshold smaller than the skew
// behaves identically, and injecting a fake clock into Go would have left
// the skew exactly where it was.
func MarkStale(db *sql.DB, threshold time.Duration) ([]string, error) {
	rows, err := db.Query(`
		UPDATE edge_registry
		SET status = 'stale'
		WHERE status = 'active'
		  AND last_heartbeat IS NOT NULL
		  AND last_heartbeat < NOW() - $1::interval
		RETURNING station_id
	`, pgInterval(threshold))
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

// pgInterval renders a Go duration as a Postgres interval literal.
//
// Microseconds because that is Postgres' interval resolution. A
// sub-microsecond threshold therefore renders as `0 microseconds`, which
// still marks every heartbeat written by an earlier transaction: NOW() is
// the current transaction's start time, so an earlier transaction's NOW()
// is strictly less than it. That is a database ordering guarantee rather
// than a race, which is the difference this whole change is about.
//
// A negative duration renders as written rather than being clamped. A
// caller passing one means something by it, and quietly turning it into
// zero would hide the mistake instead of showing it.
func pgInterval(d time.Duration) string {
	return strconv.FormatInt(d.Microseconds(), 10) + " microseconds"
}
