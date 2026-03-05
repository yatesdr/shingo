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

// ListNodesForStation returns nodes assigned to a station, including children
// of synthetic nodes assigned to the station (inherited access).
func (db *DB) ListNodesForStation(stationID string) ([]*Node, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`
		SELECT %s %s
		WHERE n.id IN (
			SELECT ns.node_id FROM node_stations ns WHERE ns.station_id = ?
		)
		OR n.parent_id IN (
			SELECT ns.node_id FROM node_stations ns WHERE ns.station_id = ?
		)
		ORDER BY n.name`, nodeSelectCols, nodeFromClause)), stationID, stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
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
