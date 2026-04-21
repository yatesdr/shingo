// Package stations holds operator_station persistence for shingo-edge.
//
// Phase 5b of the architecture plan moved the operator_stations CRUD
// out of the flat store/ package and into this sub-package. The outer
// store/ keeps a type alias (`store.OperatorStation = stations.Station`)
// and one-line delegate methods on *store.DB so external callers see
// no API change.
//
// SetStationNodes (the cross-aggregate orchestration that adds/removes
// process_nodes for a station) lives at the top-level store package
// because it spans this aggregate, the processes aggregate, and the
// orders aggregate.
package stations

import (
	"database/sql"
	"strings"
	"time"

	"shingoedge/store/internal/helpers"
)

// Station is one row of operator_stations.
type Station struct {
	ID               int64      `json:"id"`
	ProcessID        int64      `json:"process_id"`
	Code             string     `json:"code"`
	Name             string     `json:"name"`
	Note             string     `json:"note"`
	AreaLabel        string     `json:"area_label"`
	Sequence         int        `json:"sequence"`
	ControllerNodeID string     `json:"controller_node_id"`
	DeviceMode       string     `json:"device_mode"`
	Enabled          bool       `json:"enabled"`
	HealthStatus     string     `json:"health_status"`
	LastSeenAt       *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`

	ProcessName string `json:"process_name"`
}

// Input is the input shape for Create / Update.
type Input struct {
	ProcessID        int64  `json:"process_id"`
	Code             string `json:"code"`
	Name             string `json:"name"`
	Note             string `json:"note"`
	AreaLabel        string `json:"area_label"`
	Sequence         int    `json:"sequence"`
	ControllerNodeID string `json:"controller_node_id"`
	DeviceMode       string `json:"device_mode"`
	Enabled          bool   `json:"enabled"`
}

const stationSelect = `s.id, s.process_id, s.code, s.name, s.note, s.area_label, s.sequence,
	s.controller_node_id, s.device_mode, s.enabled, s.health_status,
	COALESCE(s.last_seen_at, ''), s.created_at, s.updated_at, COALESCE(p.name, '')`

const stationJoin = `FROM operator_stations s
	LEFT JOIN processes p ON p.id = s.process_id`

func scanStation(scanner interface{ Scan(...interface{}) error }) (Station, error) {
	var s Station
	var lastSeen, createdAt, updatedAt string
	err := scanner.Scan(
		&s.ID, &s.ProcessID, &s.Code, &s.Name, &s.Note, &s.AreaLabel, &s.Sequence,
		&s.ControllerNodeID, &s.DeviceMode, &s.Enabled, &s.HealthStatus,
		&lastSeen, &createdAt, &updatedAt, &s.ProcessName,
	)
	if err != nil {
		return s, err
	}
	if lastSeen != "" {
		t := helpers.ScanTime(lastSeen)
		s.LastSeenAt = &t
	}
	s.CreatedAt = helpers.ScanTime(createdAt)
	s.UpdatedAt = helpers.ScanTime(updatedAt)
	return s, nil
}

func scanStations(rows helpers.RowScanner) ([]Station, error) {
	var out []Station
	for rows.Next() {
		s, err := scanStation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// List returns every operator_stations row.
func List(db *sql.DB) ([]Station, error) {
	rows, err := db.Query(`SELECT ` + stationSelect + ` ` + stationJoin + ` ORDER BY p.name, s.sequence, s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStations(rows)
}

// ListByProcess returns operator_stations rows for one process.
func ListByProcess(db *sql.DB, processID int64) ([]Station, error) {
	rows, err := db.Query(`SELECT `+stationSelect+` `+stationJoin+` WHERE s.process_id = ? ORDER BY s.sequence, s.name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStations(rows)
}

// Get returns one operator_station by id.
func Get(db *sql.DB, id int64) (*Station, error) {
	s, err := scanStation(db.QueryRow(`SELECT `+stationSelect+` `+stationJoin+` WHERE s.id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Create inserts a station, generating code and sequence when not
// supplied.
func Create(db *sql.DB, in Input) (int64, error) {
	if in.DeviceMode == "" {
		in.DeviceMode = "fixed_hmi"
	}
	if strings.TrimSpace(in.Code) == "" {
		code, err := generateCode(db, in.ProcessID, in.Name)
		if err != nil {
			return 0, err
		}
		in.Code = code
	}
	if in.Sequence <= 0 {
		next, err := nextSequence(db, in.ProcessID)
		if err != nil {
			return 0, err
		}
		in.Sequence = next
	}
	res, err := db.Exec(`INSERT INTO operator_stations (
		process_id, code, name, note, area_label, sequence, controller_node_id, device_mode, enabled
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ProcessID, in.Code, in.Name, in.Note, in.AreaLabel, in.Sequence, in.ControllerNodeID, in.DeviceMode, in.Enabled)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Update modifies an operator_station, falling back to the existing
// code/sequence when blank.
func Update(db *sql.DB, id int64, in Input) error {
	if in.DeviceMode == "" {
		in.DeviceMode = "fixed_hmi"
	}
	if strings.TrimSpace(in.Code) == "" || in.Sequence <= 0 {
		existing, err := Get(db, id)
		if err != nil {
			return err
		}
		if strings.TrimSpace(in.Code) == "" {
			in.Code = existing.Code
		}
		if in.Sequence <= 0 {
			in.Sequence = existing.Sequence
		}
	}
	_, err := db.Exec(`UPDATE operator_stations SET
		process_id=?, code=?, name=?, note=?, area_label=?, sequence=?, controller_node_id=?, device_mode=?, enabled=?, updated_at=datetime('now')
		WHERE id=?`,
		in.ProcessID, in.Code, in.Name, in.Note, in.AreaLabel, in.Sequence, in.ControllerNodeID, in.DeviceMode, in.Enabled, id)
	return err
}

// Delete removes an operator_station.
func Delete(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM operator_stations WHERE id=?`, id)
	return err
}

// Touch updates last_seen_at and health_status on an operator_station
// (called from the HMI poll handler).
func Touch(db *sql.DB, id int64, healthStatus string) error {
	_, err := db.Exec(`UPDATE operator_stations SET health_status=?, last_seen_at=datetime('now'), updated_at=datetime('now') WHERE id=?`,
		healthStatus, id)
	return err
}

// Move swaps the sequence of a station with its neighbour in the given
// direction ("up" or "down"). No-op when at the edge.
func Move(db *sql.DB, id int64, direction string) error {
	station, err := Get(db, id)
	if err != nil {
		return err
	}
	order := "DESC"
	cmp := "<"
	if direction == "down" {
		order = "ASC"
		cmp = ">"
	}
	other, err := scanStation(db.QueryRow(
		`SELECT `+stationSelect+` `+stationJoin+` WHERE s.process_id=? AND s.sequence `+cmp+` ? ORDER BY s.sequence `+order+`, s.name LIMIT 1`,
		station.ProcessID, station.Sequence,
	))
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	_, err = db.Exec(`UPDATE operator_stations
		SET sequence = CASE
			WHEN id = ? THEN ?
			WHEN id = ? THEN ?
			ELSE sequence
		END,
		updated_at=datetime('now')
		WHERE id IN (?, ?)`,
		station.ID, other.Sequence,
		other.ID, station.Sequence,
		station.ID, other.ID,
	)
	return err
}

func nextSequence(db *sql.DB, processID int64) (int, error) {
	var maxSeq sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(sequence) FROM operator_stations WHERE process_id=?`, processID).Scan(&maxSeq); err != nil {
		return 0, err
	}
	if !maxSeq.Valid {
		return 1, nil
	}
	return int(maxSeq.Int64) + 1, nil
}

func generateCode(db *sql.DB, processID int64, name string) (string, error) {
	return helpers.GenerateUniqueCode(db, "operator_stations", "process_id", processID, helpers.SlugName(name, "station"), "station")
}

// GetNodeNames returns the core_node_name list for a station's
// process_nodes (helper used by the HMI surface to render the
// node-picker).
func GetNodeNames(db *sql.DB, stationID int64) ([]string, error) {
	rows, err := db.Query(`SELECT core_node_name FROM process_nodes
		WHERE operator_station_id=? ORDER BY sequence, name`, stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
