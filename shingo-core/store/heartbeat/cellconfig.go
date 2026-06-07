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
	CellID           string    `json:"cell_id"`
	Station          string    `json:"station"`
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
// against processes that are actually ticking, with a style/payload hint to
// recognize which is which (process_id alone is opaque).
type ProcessOption struct {
	ProcessID   int64     `json:"process_id"`
	Ticks       int64     `json:"ticks"`
	LastSeen    time.Time `json:"last_seen"`
	StyleID     int64     `json:"style_id"`
	PayloadCode string    `json:"payload_code"`
}

// ListCellConfigs returns every configured cell, ordered by cell_id.
func ListCellConfigs(db *sql.DB) ([]CellConfig, error) {
	rows, err := db.Query(`SELECT cell_id, station, primary_process_id,
		COALESCE(sub_process_ids::text,'[]'), display_name, updated_at
		FROM cell_config ORDER BY cell_id`)
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
	row := db.QueryRow(`SELECT cell_id, station, primary_process_id,
		COALESCE(sub_process_ids::text,'[]'), display_name, updated_at
		FROM cell_config WHERE cell_id=$1`, cellID)
	c, err := scanCellConfig(row)
	if err == sql.ErrNoRows {
		return CellConfig{}, false, nil
	}
	if err != nil {
		return CellConfig{}, false, err
	}
	return c, true, nil
}

// UpsertCellConfig inserts or updates a cell (keyed on cell_id), stamping
// updated_at. sub_process_ids is stored as a JSONB int array.
func UpsertCellConfig(db *sql.DB, c CellConfig) error {
	subs, err := marshalIDs(c.SubProcessIDs)
	if err != nil {
		return fmt.Errorf("marshal sub_process_ids: %w", err)
	}
	_, err = db.Exec(`INSERT INTO cell_config
		(cell_id, station, primary_process_id, sub_process_ids, display_name, updated_at)
		VALUES ($1,$2,$3,$4::jsonb,$5,NOW())
		ON CONFLICT (cell_id) DO UPDATE SET
			station=EXCLUDED.station,
			primary_process_id=EXCLUDED.primary_process_id,
			sub_process_ids=EXCLUDED.sub_process_ids,
			display_name=EXCLUDED.display_name,
			updated_at=NOW()`,
		c.CellID, c.Station, c.PrimaryProcessID, string(subs), c.DisplayName)
	return err
}

// DeleteCellConfig removes a cell. Deleting a non-existent cell is not an error.
func DeleteCellConfig(db *sql.DB, cellID string) error {
	_, err := db.Exec(`DELETE FROM cell_config WHERE cell_id=$1`, cellID)
	return err
}

// DistinctProcesses lists the Processes that have ticked for a station in the
// last 30 days, with a style/payload hint for the picker. The window keeps the
// scan partition-friendly (cell_part_events is monthly-partitioned) and hides
// long-retired processes.
func DistinctProcesses(db *sql.DB, station string) ([]ProcessOption, error) {
	rows, err := db.Query(`SELECT e.process_id, count(*) AS ticks, max(e.recorded_at) AS last_seen,
		max(e.style_id) AS style_id,
		(SELECT payload_code FROM cell_part_events e2
		   WHERE e2.cell_id=$1 AND e2.process_id=e.process_id
		   ORDER BY e2.recorded_at DESC LIMIT 1) AS payload_code
		FROM cell_part_events e
		WHERE e.cell_id=$1 AND e.recorded_at >= NOW() - INTERVAL '30 days'
		GROUP BY e.process_id
		ORDER BY ticks DESC`, station)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProcessOption{}
	for rows.Next() {
		var p ProcessOption
		var payload sql.NullString
		if err := rows.Scan(&p.ProcessID, &p.Ticks, &p.LastSeen, &p.StyleID, &payload); err != nil {
			return nil, err
		}
		p.PayloadCode = payload.String
		out = append(out, p)
	}
	return out, rows.Err()
}

// scanCellConfig reads one row (from Query or QueryRow) into a CellConfig,
// decoding the JSONB sub_process_ids.
func scanCellConfig(s interface{ Scan(...any) error }) (CellConfig, error) {
	var c CellConfig
	var subs string
	if err := s.Scan(&c.CellID, &c.Station, &c.PrimaryProcessID, &subs, &c.DisplayName, &c.UpdatedAt); err != nil {
		return CellConfig{}, err
	}
	ids, err := unmarshalIDs([]byte(subs))
	if err != nil {
		return CellConfig{}, fmt.Errorf("decode sub_process_ids for cell %q: %w", c.CellID, err)
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
