package heartbeat

// cell_config persistence (Phase E, Q-025). A cell is an operator-defined
// grouping of production Processes: one primary Process plus optional
// sub-Processes. Process ids match cell_part_events.process_id (the Process
// grain the PLC counters tick at). The SQL shell lives here; the analytical
// split over a cell's event stream is the pure ComputeResolvedCellState in
// cellstate.go.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// CellConfig is one operator-defined cell (a cell_config row).
type CellConfig struct {
	CellLabel        string    `json:"cell_id"`
	StationUID       string    `json:"station"`
	PrimaryProcessID int64     `json:"primary_process_id"`
	SubProcessIDs    []int64   `json:"sub_process_ids"`
	DisplayName      string    `json:"display_name"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// AllProcessIDs returns the primary plus every sub process id — the full set a
// cell's state query filters cell_part_events to.
func (c CellConfig) AllProcessIDs() []int64 {
	out := make([]int64, 0, 1+len(c.SubProcessIDs))
	out = append(out, c.PrimaryProcessID)
	out = append(out, c.SubProcessIDs...)
	return out
}

// ProcessOption is one selectable Process for the /admin/cells picker —
// surfaced from the live cell_part_events stream so the operator configures
// against processes that are actually ticking, with a style and tick-count
// hint to recognize which is which (process_id alone is opaque).
//
// No payload hint: a tick has no single payload (one stroke on a reporting
// point fans out to every node scope the style claims), and the column the
// hint read was never written.
type ProcessOption struct {
	ProcessID int64     `json:"process_id"`
	Ticks     int64     `json:"ticks"`
	LastSeen  time.Time `json:"last_seen"`
	StyleID   int64     `json:"style_id"`
}

// ListCellConfigs returns every configured cell, ordered by cell_label.
func ListCellConfigs(db *sql.DB) ([]CellConfig, error) {
	rows, err := db.Query(`SELECT cell_label, station_uid, primary_process_id,
		COALESCE(sub_process_ids::text,'[]'), display_name, updated_at
		FROM cell_config ORDER BY cell_label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CellConfig{}
	for rows.Next() {
		c, err := scanCellConfig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCellConfig returns one cell by id. The bool is false (no error) when the
// cell isn't configured.
func GetCellConfig(db *sql.DB, cellID string) (CellConfig, bool, error) {
	row := db.QueryRow(`SELECT cell_label, station_uid, primary_process_id,
		COALESCE(sub_process_ids::text,'[]'), display_name, updated_at
		FROM cell_config WHERE cell_label=$1`, cellID)
	c, err := scanCellConfig(row)
	if err == sql.ErrNoRows {
		return CellConfig{}, false, nil
	}
	if err != nil {
		return CellConfig{}, false, err
	}
	return c, true, nil
}

// UpsertCellConfig inserts or updates a cell (keyed on cell_label), stamping
// updated_at. sub_process_ids is stored as a JSONB int array.
func UpsertCellConfig(db *sql.DB, c CellConfig) error {
	subs, err := marshalIDs(c.SubProcessIDs)
	if err != nil {
		return fmt.Errorf("marshal sub_process_ids: %w", err)
	}
	_, err = db.Exec(`INSERT INTO cell_config
		(cell_label, station_uid, primary_process_id, sub_process_ids, display_name, updated_at)
		VALUES ($1,$2,$3,$4::jsonb,$5,NOW())
		ON CONFLICT (cell_label) DO UPDATE SET
			station_uid=EXCLUDED.station_uid,
			primary_process_id=EXCLUDED.primary_process_id,
			sub_process_ids=EXCLUDED.sub_process_ids,
			display_name=EXCLUDED.display_name,
			updated_at=NOW()`,
		c.CellLabel, c.StationUID, c.PrimaryProcessID, string(subs), c.DisplayName)
	return err
}

// DeleteCellConfig removes a cell. Deleting a non-existent cell is not an error.
func DeleteCellConfig(db *sql.DB, cellID string) error {
	_, err := db.Exec(`DELETE FROM cell_config WHERE cell_label=$1`, cellID)
	return err
}

// DistinctProcesses lists the Processes that have ticked for a station in the
// last 30 days, with a style hint for the picker. The window keeps the scan
// partition-friendly (cell_part_events is monthly-partitioned) and hides
// long-retired processes.
func DistinctProcesses(db *sql.DB, station string) ([]ProcessOption, error) {
	rows, err := db.Query(`SELECT e.process_id, count(*) AS ticks, max(e.recorded_at) AS last_seen,
		max(e.style_id) AS style_id
		FROM cell_part_events e
		WHERE e.station_uid=$1 AND e.recorded_at >= NOW() - INTERVAL '30 days'
		GROUP BY e.process_id
		ORDER BY ticks DESC`, station)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProcessOption{}
	for rows.Next() {
		var p ProcessOption
		if err := rows.Scan(&p.ProcessID, &p.Ticks, &p.LastSeen, &p.StyleID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// scanCellConfig reads one row (from Query or QueryRow) into a CellConfig,
// decoding the JSONB sub_process_ids.
func scanCellConfig(s interface{ Scan(...any) error }) (CellConfig, error) {
	var c CellConfig
	var subs string
	if err := s.Scan(&c.CellLabel, &c.StationUID, &c.PrimaryProcessID, &subs, &c.DisplayName, &c.UpdatedAt); err != nil {
		return CellConfig{}, err
	}
	ids, err := unmarshalIDs([]byte(subs))
	if err != nil {
		return CellConfig{}, fmt.Errorf("decode sub_process_ids for cell %q: %w", c.CellLabel, err)
	}
	c.SubProcessIDs = ids
	return c, nil
}

func marshalIDs(ids []int64) ([]byte, error) {
	if ids == nil {
		ids = []int64{}
	}
	return json.Marshal(ids)
}

func unmarshalIDs(b []byte) ([]int64, error) {
	if len(b) == 0 {
		return []int64{}, nil
	}
	var ids []int64
	if err := json.Unmarshal(b, &ids); err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []int64{}
	}
	return ids, nil
}
