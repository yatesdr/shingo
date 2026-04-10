package store

import "time"

// DemandRegistryEntry maps a payload code to a manual_swap node that accepts it.
// Synced from Edge claim config via the ClaimSync protocol message.
type DemandRegistryEntry struct {
	ID             int64     `json:"id"`
	StationID      string    `json:"station_id"`
	CoreNodeName   string    `json:"core_node_name"`
	Role           string    `json:"role"`           // "produce" or "consume"
	PayloadCode    string    `json:"payload_code"`
	OutboundDest   string    `json:"outbound_dest"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// SyncDemandRegistry replaces all registry entries for a station with the given entries.
// Uses a delete+insert pattern inside a transaction for atomicity.
func (db *DB) SyncDemandRegistry(stationID string, entries []DemandRegistryEntry) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Remove existing entries for this station
	if _, err := tx.Exec(`DELETE FROM demand_registry WHERE station_id = $1`, stationID); err != nil {
		return err
	}

	// Insert new entries
	for _, e := range entries {
		if _, err := tx.Exec(`INSERT INTO demand_registry (station_id, core_node_name, role, payload_code, outbound_dest) VALUES ($1, $2, $3, $4, $5)`,
			stationID, e.CoreNodeName, e.Role, e.PayloadCode, e.OutboundDest); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// LookupDemandRegistry returns all registry entries for a given payload code.
// Used by the kanban wiring to find which nodes need orders when a bin event fires.
func (db *DB) LookupDemandRegistry(payloadCode string) ([]DemandRegistryEntry, error) {
	rows, err := db.Query(`SELECT id, station_id, core_node_name, role, payload_code, outbound_dest, updated_at
		FROM demand_registry WHERE payload_code = $1`, payloadCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []DemandRegistryEntry
	for rows.Next() {
		var e DemandRegistryEntry
		if err := rows.Scan(&e.ID, &e.StationID, &e.CoreNodeName, &e.Role, &e.PayloadCode, &e.OutboundDest, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ListDemandRegistry returns all entries in the demand registry.
func (db *DB) ListDemandRegistry() ([]DemandRegistryEntry, error) {
	rows, err := db.Query(`SELECT id, station_id, core_node_name, role, payload_code, outbound_dest, updated_at
		FROM demand_registry ORDER BY station_id, core_node_name, payload_code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []DemandRegistryEntry
	for rows.Next() {
		var e DemandRegistryEntry
		if err := rows.Scan(&e.ID, &e.StationID, &e.CoreNodeName, &e.Role, &e.PayloadCode, &e.OutboundDest, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
