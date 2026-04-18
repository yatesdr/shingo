package store

// Stage 2D delegate file: node_stations CRUD + effective resolution live in
// store/nodes/.

import "shingocore/store/nodes"

func (db *DB) AssignNodeToStation(nodeID int64, stationID string) error {
	return nodes.AssignStation(db.DB, nodeID, stationID)
}

func (db *DB) UnassignNodeFromStation(nodeID int64, stationID string) error {
	return nodes.UnassignStation(db.DB, nodeID, stationID)
}

func (db *DB) ListStationsForNode(nodeID int64) ([]string, error) {
	return nodes.ListStationsForNode(db.DB, nodeID)
}

// ListNodesForStation returns nodes directly assigned to a station, plus
// NGRP node group parents whose children are assigned to the station.
func (db *DB) ListNodesForStation(stationID string) ([]*Node, error) {
	return nodes.ListNodesForStation(db.DB, stationID)
}

// GetEffectiveStations resolves the station set for a node via its
// station_mode property (all / none / specific / inherit).
func (db *DB) GetEffectiveStations(nodeID int64) ([]string, error) {
	return nodes.GetEffectiveStations(db.DB, nodeID)
}

// SetNodeStations replaces all station assignments for a node.
func (db *DB) SetNodeStations(nodeID int64, stationIDs []string) error {
	return nodes.SetStations(db.DB, nodeID, stationIDs)
}
