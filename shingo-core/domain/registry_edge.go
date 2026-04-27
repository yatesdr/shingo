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
}
