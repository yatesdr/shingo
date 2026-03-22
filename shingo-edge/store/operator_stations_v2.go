package store

import (
	"database/sql"
	"time"
)

type OperatorStation struct {
	ID                 int64      `json:"id"`
	ProcessID          int64      `json:"process_id"`
	ParentStationID    *int64     `json:"parent_station_id,omitempty"`
	Code               string     `json:"code"`
	Name               string     `json:"name"`
	AreaLabel          string     `json:"area_label"`
	Sequence           int        `json:"sequence"`
	ControllerNodeID   string     `json:"controller_node_id"`
	DeviceMode         string     `json:"device_mode"`
	ExpectedClientType string     `json:"expected_client_type"`
	Enabled            bool       `json:"enabled"`
	HealthStatus       string     `json:"health_status"`
	LastSeenAt         *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`

	ProcessName       string `json:"process_name"`
	ParentStationName string `json:"parent_station_name"`
	HierarchyPath     string `json:"hierarchy_path"`
}

type OperatorStationInput struct {
	ProcessID        int64  `json:"process_id"`
	ParentStationID  *int64 `json:"parent_station_id"`
	Code             string `json:"code"`
	Name             string `json:"name"`
	AreaLabel        string `json:"area_label"`
	Sequence         int    `json:"sequence"`
	ControllerNodeID string `json:"controller_node_id"`
	DeviceMode       string `json:"device_mode"`
	Enabled          bool   `json:"enabled"`
}

func scanStations(rows rowScanner) ([]OperatorStation, error) {
	var out []OperatorStation
	for rows.Next() {
		s, err := scanStation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Next() bool
	Scan(...interface{}) error
	Err() error
}

func scanStation(scanner interface{ Scan(...interface{}) error }) (OperatorStation, error) {
	var s OperatorStation
	var parentStationID sql.NullInt64
	var lastSeen, createdAt, updatedAt string
	err := scanner.Scan(
		&s.ID, &s.ProcessID, &parentStationID, &s.Code, &s.Name, &s.AreaLabel, &s.Sequence,
		&s.ControllerNodeID, &s.DeviceMode, &s.ExpectedClientType, &s.Enabled, &s.HealthStatus,
		&lastSeen, &createdAt, &updatedAt, &s.ProcessName, &s.ParentStationName,
	)
	if err != nil {
		return s, err
	}
	if parentStationID.Valid {
		s.ParentStationID = &parentStationID.Int64
	}
	if lastSeen != "" {
		t := scanTime(lastSeen)
		s.LastSeenAt = &t
	}
	s.CreatedAt = scanTime(createdAt)
	s.UpdatedAt = scanTime(updatedAt)
	s.HierarchyPath = s.Name
	if s.ParentStationName != "" {
		s.HierarchyPath = s.ParentStationName + " / " + s.Name
	}
	return s, nil
}

const stationSelect = `s.id, s.process_id, s.parent_station_id, s.code, s.name, s.area_label, s.sequence,
	s.controller_node_id, s.device_mode, s.expected_client_type, s.enabled, s.health_status,
	COALESCE(s.last_seen_at, ''), s.created_at, s.updated_at, COALESCE(p.name, ''), COALESCE(ps.name, '')`

const stationJoin = `FROM operator_stations s
	LEFT JOIN processes p ON p.id = s.process_id
	LEFT JOIN operator_stations ps ON ps.id = s.parent_station_id`

func (db *DB) ListOperatorStations() ([]OperatorStation, error) {
	rows, err := db.Query(`SELECT ` + stationSelect + ` ` + stationJoin + ` ORDER BY p.name, s.sequence, s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stations, err := scanStations(rows)
	if err != nil {
		return nil, err
	}
	return hydrateStationPaths(stations), nil
}

func (db *DB) ListOperatorStationsByProcess(processID int64) ([]OperatorStation, error) {
	rows, err := db.Query(`SELECT `+stationSelect+` `+stationJoin+` WHERE s.process_id = ? ORDER BY s.sequence, s.name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stations, err := scanStations(rows)
	if err != nil {
		return nil, err
	}
	return hydrateStationPaths(stations), nil
}

func (db *DB) GetOperatorStation(id int64) (*OperatorStation, error) {
	s, err := scanStation(db.QueryRow(`SELECT `+stationSelect+` `+stationJoin+` WHERE s.id = ?`, id))
	if err != nil {
		return nil, err
	}
	if s.ParentStationID != nil {
		if stations, err := db.ListOperatorStationsByProcess(s.ProcessID); err == nil {
			for i := range stations {
				if stations[i].ID == s.ID {
					s.HierarchyPath = stations[i].HierarchyPath
					break
				}
			}
		}
	}
	if s.HierarchyPath == "" {
		s.HierarchyPath = s.Name
	}
	return &s, nil
}

func (db *DB) CreateOperatorStation(in OperatorStationInput) (int64, error) {
	if in.DeviceMode == "" {
		in.DeviceMode = "fixed_hmi"
	}
	res, err := db.Exec(`INSERT INTO operator_stations (
		process_id, parent_station_id, code, name, area_label, sequence, controller_node_id, device_mode, enabled
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ProcessID, in.ParentStationID, in.Code, in.Name, in.AreaLabel, in.Sequence, in.ControllerNodeID, in.DeviceMode, in.Enabled)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateOperatorStation(id int64, in OperatorStationInput) error {
	if in.DeviceMode == "" {
		in.DeviceMode = "fixed_hmi"
	}
	_, err := db.Exec(`UPDATE operator_stations SET
		process_id=?, parent_station_id=?, code=?, name=?, area_label=?, sequence=?, controller_node_id=?, device_mode=?, enabled=?, updated_at=datetime('now')
		WHERE id=?`,
		in.ProcessID, in.ParentStationID, in.Code, in.Name, in.AreaLabel, in.Sequence, in.ControllerNodeID, in.DeviceMode, in.Enabled, id)
	return err
}

func (db *DB) DeleteOperatorStation(id int64) error {
	_, err := db.Exec(`DELETE FROM operator_stations WHERE id=?`, id)
	return err
}

func (db *DB) TouchOperatorStation(id int64, healthStatus string) error {
	_, err := db.Exec(`UPDATE operator_stations SET health_status=?, last_seen_at=datetime('now'), updated_at=datetime('now') WHERE id=?`,
		healthStatus, id)
	return err
}

func hydrateStationPaths(stations []OperatorStation) []OperatorStation {
	byID := make(map[int64]*OperatorStation, len(stations))
	for i := range stations {
		byID[stations[i].ID] = &stations[i]
	}
	var resolve func(*OperatorStation) string
	resolve = func(s *OperatorStation) string {
		if s == nil {
			return ""
		}
		if s.HierarchyPath != "" && s.HierarchyPath != s.Name {
			return s.HierarchyPath
		}
		if s.ParentStationID != nil {
			if parent := byID[*s.ParentStationID]; parent != nil {
				s.HierarchyPath = resolve(parent) + " / " + s.Name
				return s.HierarchyPath
			}
		}
		s.HierarchyPath = s.Name
		return s.HierarchyPath
	}
	for i := range stations {
		resolve(&stations[i])
	}
	return stations
}
