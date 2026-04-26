package store

// Phase 5b delegate file: operator_station CRUD lives in
// store/stations/. This file preserves the *store.DB method surface
// so external callers do not need to change.
//
// Phase 6.0b extracted SetStationNodes (cross-aggregate orchestration:
// stations + processes + orders) to its own top-level file
// station_nodes.go.

import (
	"shingoedge/store/stations"
)

// ListOperatorStations returns every operator_stations row.
func (db *DB) ListOperatorStations() ([]stations.Station, error) {
	return stations.List(db.DB)
}

// ListOperatorStationsByProcess returns operator_stations rows for one
// process.
func (db *DB) ListOperatorStationsByProcess(processID int64) ([]stations.Station, error) {
	return stations.ListByProcess(db.DB, processID)
}

// GetOperatorStation returns one operator_station by id.
func (db *DB) GetOperatorStation(id int64) (*stations.Station, error) {
	return stations.Get(db.DB, id)
}

// CreateOperatorStation inserts a station, generating code and
// sequence when not supplied.
func (db *DB) CreateOperatorStation(in stations.Input) (int64, error) {
	return stations.Create(db.DB, in)
}

// UpdateOperatorStation modifies an operator_station.
func (db *DB) UpdateOperatorStation(id int64, in stations.Input) error {
	return stations.Update(db.DB, id, in)
}

// DeleteOperatorStation removes an operator_station.
func (db *DB) DeleteOperatorStation(id int64) error {
	return stations.Delete(db.DB, id)
}

// TouchOperatorStation updates last_seen_at and health_status.
func (db *DB) TouchOperatorStation(id int64, healthStatus string) error {
	return stations.Touch(db.DB, id, healthStatus)
}

// MoveOperatorStation swaps the sequence of a station with its
// neighbour in the given direction ("up" or "down").
func (db *DB) MoveOperatorStation(id int64, direction string) error {
	return stations.Move(db.DB, id, direction)
}

// GetStationNodeNames returns the core_node_name list for a station.
func (db *DB) GetStationNodeNames(stationID int64) ([]string, error) {
	return stations.GetNodeNames(db.DB, stationID)
}

// SetStationNodes lives at top-level store/station_nodes.go.
