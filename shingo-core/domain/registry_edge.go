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
	ID int64 `json:"id"`
	// StationUID is the identity Core minted at enrollment and correlates all
	// history to. It never changes — not when the display name changes, not
	// when the hardware is replaced.
	StationUID string `json:"station_uid"`
	// DisplayName is the operator's string, e.g. "SPRINGFIELD / EDGE-2".
	// Freely editable, read by nothing but a human, and NEVER an identifier on
	// the wire. Everything that made a rename dangerous lived in the fact that
	// this and StationUID used to be one column.
	DisplayName string `json:"display_name"`
	// StationID is the transport routing selector — protocol.Address.Station.
	// Its value IS StationUID.
	StationID     string     `json:"station_id"`
	Hostname      string     `json:"hostname"`
	Version       string     `json:"version"`
	RegisteredAt  time.Time  `json:"registered_at"`
	LastHeartbeat *time.Time `json:"last_heartbeat"`
	Status        string     `json:"status"`

	// BoundHostname is the first hostname to have registered this station.
	// Hostname above is LAST-SEEN and is overwritten on every register; this
	// one is not, which is what makes a second machine visible at all.
	BoundHostname string `json:"bound_hostname"`
	// BoundInstance is the per-process id of the edge that currently holds the
	// lease; PrevInstance is the one it displaced. A single Pi never reuses an
	// instance it has been displaced from — every boot draws a fresh value and
	// a live process reuses the one it holds — so PrevInstance coming back is
	// the signature of two live machines, including the SD-card-clone case the
	// hostname check cannot see.
	BoundInstance string     `json:"bound_instance"`
	PrevInstance  string     `json:"prev_instance"`
	BoundAt       *time.Time `json:"bound_at"`
	// ClaimedAt is NULL until a human has said what this station is. An edge
	// may introduce itself and run; only a person can say WHICH station it is.
	// Never cleared — it records whether anybody ever looked, not current state.
	ClaimedAt *time.Time `json:"claimed_at"`
	// ConflictHostname / ConflictCount / ConflictAt record the most recent
	// register that conflicted with the binding. Count 0 means it has never
	// happened. A count that keeps climbing means two machines are alive and
	// taking turns; a count frozen at a small number with an old ConflictAt
	// means the station moved boxes once and nobody ran registry.Rebind.
	ConflictHostname string     `json:"conflict_hostname"`
	ConflictCount    int64      `json:"conflict_count"`
	ConflictAt       *time.Time `json:"conflict_at"`
}
