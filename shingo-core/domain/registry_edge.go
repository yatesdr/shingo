package domain

import "time"

// RegistryEdge is a row in the edge_registry table — one entry per
// shingo-edge instance that has registered with core. Tracks
// hostname, version, line assignments, and heartbeat status so the
// admin UI and registry service can show which edges are up.
//
// Stage 2A.2 lifted this struct into domain/ so handlers and the
// node-page builder can return registered-edge data without
// importing shingo-core/store/registry. The store package
// re-exports the type via `type Edge = domain.RegistryEdge`.
type RegistryEdge struct {
	ID            int64      `json:"id"`
	StationID     string     `json:"station_id"`
	Hostname      string     `json:"hostname"`
	Version       string     `json:"version"`
	LineIDs       []string   `json:"line_ids"`
	RegisteredAt  time.Time  `json:"registered_at"`
	LastHeartbeat *time.Time `json:"last_heartbeat"`
	Status        string     `json:"status"`

	// BoundHostname is the first hostname to have registered this station id.
	// Hostname above is LAST-SEEN and is overwritten on every register; this
	// one is not, which is what makes a second machine visible at all.
	BoundHostname string `json:"bound_hostname"`
	// ConflictHostname / ConflictCount / ConflictAt record the most recent
	// register that arrived from a machine other than BoundHostname. Count 0
	// means it has never happened. A count that keeps climbing means two
	// machines are alive and taking turns; a count frozen at a small number
	// with an old ConflictAt means the station moved boxes once and nobody
	// ran registry.Rebind.
	ConflictHostname string     `json:"conflict_hostname"`
	ConflictCount    int64      `json:"conflict_count"`
	ConflictAt       *time.Time `json:"conflict_at"`
}
