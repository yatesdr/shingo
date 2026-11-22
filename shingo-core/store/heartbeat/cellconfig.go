package heartbeat

// The cell shape (Phase E, Q-025). A cell is one primary production Process
// plus optional sub-Processes; process ids match cell_part_events.process_id
// (the Process grain the PLC counters tick at). Every cell is derived from the
// catalog an Edge reported (edge_cells, service.deriveCellConfig): since v189
// nothing stores one. The analytical split over a cell's event stream is the
// pure ComputeResolvedCellState in cellstate.go.

import "time"

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
