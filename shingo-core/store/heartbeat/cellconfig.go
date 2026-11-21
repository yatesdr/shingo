package heartbeat

// The cell shape (Phase E, Q-025). A cell is one primary production Process
// plus optional sub-Processes; process ids match cell_part_events.process_id
// (the Process grain the PLC counters tick at). Every cell is derived from the
// catalog an Edge reported (edge_cells, service.deriveCellConfig): since v189
// nothing stores one. The analytical split over a cell's event stream is the
// pure ComputeResolvedCellState in cellstate.go.

import (
	"database/sql"
	"time"
)

// CellConfig is one cell, as every heartbeat surface serves it. Its JSON is
// the /api/cells row (TestCellsAPI_CatalogCellWithNoConfigRow pins it, the
// zero updated_at included: no cell is stored, so none has a save time).
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

// ProcessOption is one Process ticking for a station, from the live
// cell_part_events stream, with a style and tick-count hint to recognize which
// is which (process_id alone is opaque). It was the /admin/cells picker's
// option; that page is retired, and GET /api/cells/processes, which serves it,
// has no page reader.
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

// DistinctProcesses lists the Processes that have ticked for a station in the
// last 30 days, with a style hint. The window keeps the scan
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
