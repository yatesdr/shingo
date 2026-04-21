package store

// Phase 5b delegate file: operator_station CRUD now lives in
// store/stations/. This file preserves the *store.DB method surface
// so external callers do not need to change.
//
// SetStationNodes stays here as a cross-aggregate orchestration — it
// spans stations, processes (process_nodes + runtime rows), and
// orders (to decide enable-vs-delete for nodes with in-flight work).

import (
	"strings"

	"shingoedge/store/stations"
)

// OperatorStation is one row of operator_stations.
type OperatorStation = stations.Station

// OperatorStationInput is the input shape for CreateOperatorStation /
// UpdateOperatorStation.
type OperatorStationInput = stations.Input

// ListOperatorStations returns every operator_stations row.
func (db *DB) ListOperatorStations() ([]OperatorStation, error) {
	return stations.List(db.DB)
}

// ListOperatorStationsByProcess returns operator_stations rows for one
// process.
func (db *DB) ListOperatorStationsByProcess(processID int64) ([]OperatorStation, error) {
	return stations.ListByProcess(db.DB, processID)
}

// GetOperatorStation returns one operator_station by id.
func (db *DB) GetOperatorStation(id int64) (*OperatorStation, error) {
	return stations.Get(db.DB, id)
}

// CreateOperatorStation inserts a station, generating code and
// sequence when not supplied.
func (db *DB) CreateOperatorStation(in OperatorStationInput) (int64, error) {
	return stations.Create(db.DB, in)
}

// UpdateOperatorStation modifies an operator_station.
func (db *DB) UpdateOperatorStation(id int64, in OperatorStationInput) error {
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

// SetStationNodes syncs process_nodes for a station to match the given
// core node names. Cross-aggregate: it reaches into the processes
// aggregate (process_nodes + runtime rows) and into orders to decide
// enable-vs-delete for nodes with in-flight work.
//
// Stays at the top-level store package because the orchestration
// spans multiple aggregates that each own their own sub-package.
func (db *DB) SetStationNodes(stationID int64, nodeNames []string) error {
	station, err := db.GetOperatorStation(stationID)
	if err != nil {
		return err
	}

	existing, err := db.ListProcessNodesByStation(stationID)
	if err != nil {
		return err
	}

	existingMap := map[string]ProcessNode{}
	for _, n := range existing {
		existingMap[n.CoreNodeName] = n
	}

	// Normalize input: trim and deduplicate, preserving order.
	clean := make([]string, 0, len(nodeNames))
	desired := map[string]bool{}
	for _, name := range nodeNames {
		name = strings.TrimSpace(name)
		if name != "" && !desired[name] {
			desired[name] = true
			clean = append(clean, name)
		}
	}

	for i, name := range clean {
		if _, exists := existingMap[name]; exists {
			if _, err := db.Exec(`UPDATE process_nodes SET sequence=?, enabled=1, updated_at=datetime('now')
				WHERE operator_station_id=? AND core_node_name=?`, i+1, stationID, name); err != nil {
				return err
			}
			continue
		}
		id, err := db.CreateProcessNode(ProcessNodeInput{
			ProcessID:         station.ProcessID,
			OperatorStationID: &stationID,
			CoreNodeName:      name,
			Name:              name,
			Sequence:          i + 1,
			Enabled:           true,
		})
		if err != nil {
			return err
		}
		if _, err := db.EnsureProcessNodeRuntime(id); err != nil {
			return err
		}
	}

	for _, n := range existing {
		if desired[n.CoreNodeName] {
			continue
		}
		active, err := db.ListActiveOrdersByProcessNode(n.ID)
		if err != nil {
			return err
		}
		if len(active) > 0 {
			if _, err := db.Exec(`UPDATE process_nodes SET enabled=0, updated_at=datetime('now') WHERE id=?`, n.ID); err != nil {
				return err
			}
			continue
		}
		if err := db.DeleteProcessNode(n.ID); err != nil {
			return err
		}
	}

	return nil
}
