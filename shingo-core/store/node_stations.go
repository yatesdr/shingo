package store

import "fmt"

func (db *DB) AssignNodeToStation(nodeID int64, stationID string) error {
	_, err := db.Exec(db.Q(`INSERT INTO node_stations (node_id, station_id) VALUES (?, ?) ON CONFLICT DO NOTHING`), nodeID, stationID)
	return err
}

func (db *DB) UnassignNodeFromStation(nodeID int64, stationID string) error {
	_, err := db.Exec(db.Q(`DELETE FROM node_stations WHERE node_id=? AND station_id=?`), nodeID, stationID)
	return err
}

func (db *DB) ListStationsForNode(nodeID int64) ([]string, error) {
	rows, err := db.Query(db.Q(`SELECT station_id FROM node_stations WHERE node_id=? ORDER BY station_id`), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stations []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		stations = append(stations, s)
	}
	return stations, rows.Err()
}

// ListNodesForStation returns nodes directly assigned to a station.
// Includes top-level nodes and non-synthetic direct children of NGRP node groups.
func (db *DB) ListNodesForStation(stationID string) ([]*Node, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`
		SELECT %s %s
		WHERE n.id IN (
			SELECT ns.node_id FROM node_stations ns WHERE ns.station_id = ?
		)
		AND (n.parent_id IS NULL
		     OR (n.is_synthetic = 0 AND EXISTS (
		         SELECT 1 FROM nodes p
		         JOIN node_types pt ON pt.id = p.node_type_id
		         WHERE p.id = n.parent_id AND pt.code = 'NGRP'
		     )))
		ORDER BY n.name`, nodeSelectCols, nodeFromClause)), stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetEffectiveStations returns stations for a node based on its station_mode property:
//   - "all": no restrictions (returns nil)
//   - "specific": returns directly assigned stations
//   - "" / "inherit": walks parent chain until a non-empty set is found
func (db *DB) GetEffectiveStations(nodeID int64) ([]string, error) {
	mode := db.GetNodeProperty(nodeID, "station_mode")
	switch mode {
	case "all":
		return nil, nil
	case "none":
		return []string{}, nil // empty = no stations permitted
	case "specific":
		return db.ListStationsForNode(nodeID)
	default: // "" or "inherit"
		cur := nodeID
		for {
			stations, err := db.ListStationsForNode(cur)
			if err != nil {
				return nil, err
			}
			if len(stations) > 0 {
				return stations, nil
			}
			node, err := db.GetNode(cur)
			if err != nil {
				return nil, nil
			}
			if node.ParentID == nil {
				return nil, nil
			}
			cur = *node.ParentID
		}
	}
}

// SetNodeStations replaces all station assignments for a node.
func (db *DB) SetNodeStations(nodeID int64, stationIDs []string) error {
	if _, err := db.Exec(db.Q(`DELETE FROM node_stations WHERE node_id=?`), nodeID); err != nil {
		return err
	}
	for _, sid := range stationIDs {
		if _, err := db.Exec(db.Q(`INSERT INTO node_stations (node_id, station_id) VALUES (?, ?)`), nodeID, sid); err != nil {
			return err
		}
	}
	return nil
}
