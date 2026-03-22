package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"
)

type OperatorStation struct {
	ID                 int64      `json:"id"`
	ProcessID          int64      `json:"process_id"`
	Code               string     `json:"code"`
	Name               string     `json:"name"`
	Note               string     `json:"note"`
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

	ProcessName string `json:"process_name"`
}

type OperatorStationInput struct {
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
	var lastSeen, createdAt, updatedAt string
	err := scanner.Scan(
		&s.ID, &s.ProcessID, &s.Code, &s.Name, &s.Note, &s.AreaLabel, &s.Sequence,
		&s.ControllerNodeID, &s.DeviceMode, &s.ExpectedClientType, &s.Enabled, &s.HealthStatus,
		&lastSeen, &createdAt, &updatedAt, &s.ProcessName,
	)
	if err != nil {
		return s, err
	}
	if lastSeen != "" {
		t := scanTime(lastSeen)
		s.LastSeenAt = &t
	}
	s.CreatedAt = scanTime(createdAt)
	s.UpdatedAt = scanTime(updatedAt)
	return s, nil
}

const stationSelect = `s.id, s.process_id, s.code, s.name, s.note, s.area_label, s.sequence,
	s.controller_node_id, s.device_mode, s.expected_client_type, s.enabled, s.health_status,
	COALESCE(s.last_seen_at, ''), s.created_at, s.updated_at, COALESCE(p.name, '')`

const stationJoin = `FROM operator_stations s
	LEFT JOIN processes p ON p.id = s.process_id`

func (db *DB) ListOperatorStations() ([]OperatorStation, error) {
	rows, err := db.Query(`SELECT ` + stationSelect + ` ` + stationJoin + ` ORDER BY p.name, s.sequence, s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStations(rows)
}

func (db *DB) ListOperatorStationsByProcess(processID int64) ([]OperatorStation, error) {
	rows, err := db.Query(`SELECT `+stationSelect+` `+stationJoin+` WHERE s.process_id = ? ORDER BY s.sequence, s.name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStations(rows)
}

func (db *DB) GetOperatorStation(id int64) (*OperatorStation, error) {
	s, err := scanStation(db.QueryRow(`SELECT `+stationSelect+` `+stationJoin+` WHERE s.id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (db *DB) CreateOperatorStation(in OperatorStationInput) (int64, error) {
	if in.DeviceMode == "" {
		in.DeviceMode = "fixed_hmi"
	}
	if strings.TrimSpace(in.Code) == "" {
		code, err := db.generateOperatorStationCode(in.ProcessID, in.Name)
		if err != nil {
			return 0, err
		}
		in.Code = code
	}
	if in.Sequence <= 0 {
		next, err := db.nextOperatorStationSequence(in.ProcessID)
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

func (db *DB) UpdateOperatorStation(id int64, in OperatorStationInput) error {
	if in.DeviceMode == "" {
		in.DeviceMode = "fixed_hmi"
	}
	if strings.TrimSpace(in.Code) == "" {
		existing, err := db.GetOperatorStation(id)
		if err != nil {
			return err
		}
		in.Code = existing.Code
		if in.Sequence <= 0 {
			in.Sequence = existing.Sequence
		}
	}
	if in.Sequence <= 0 {
		existing, err := db.GetOperatorStation(id)
		if err != nil {
			return err
		}
		in.Sequence = existing.Sequence
	}
	_, err := db.Exec(`UPDATE operator_stations SET
		process_id=?, code=?, name=?, note=?, area_label=?, sequence=?, controller_node_id=?, device_mode=?, enabled=?, updated_at=datetime('now')
		WHERE id=?`,
		in.ProcessID, in.Code, in.Name, in.Note, in.AreaLabel, in.Sequence, in.ControllerNodeID, in.DeviceMode, in.Enabled, id)
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

func (db *DB) MoveOperatorStation(id int64, direction string) error {
	station, err := db.GetOperatorStation(id)
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

func (db *DB) nextOperatorStationSequence(processID int64) (int, error) {
	var maxSeq sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(sequence) FROM operator_stations WHERE process_id=?`, processID).Scan(&maxSeq); err != nil {
		return 0, err
	}
	if !maxSeq.Valid {
		return 1, nil
	}
	return int(maxSeq.Int64) + 1, nil
}

func (db *DB) generateOperatorStationCode(processID int64, name string) (string, error) {
	base := slugOperatorStationName(name)
	for i := 1; i <= 9999; i++ {
		candidate := base
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", base, i)
		}
		var exists int
		err := db.QueryRow(`SELECT 1 FROM operator_stations WHERE process_id=? AND code=? LIMIT 1`, processID, candidate).Scan(&exists)
		if err == sql.ErrNoRows {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not generate unique station code")
}

func slugOperatorStationName(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return "station"
	}
	var b strings.Builder
	prevDash := false
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "station"
	}
	return out
}
