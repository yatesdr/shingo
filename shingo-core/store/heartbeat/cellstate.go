package heartbeat

// Resolved cell state (Phase E, Q-025): the per-Process split of a cell's
// event stream into a primary rhythm plus optional sub-Process rhythms. This
// is the pure functional core — ComputeResolvedCellState takes a cell's events
// + its config and returns the split with no DB access, so it's unit-testable
// without Postgres (cf. ComputeCellState in heartbeat.go).

import "time"

// ProcessState is one Process's live state within a cell. CellState is embedded
// so its fields (state, last_fire, current_cycle_ms, …) flatten into the JSON
// alongside process_id.
type ProcessState struct {
	ProcessID int64 `json:"process_id"`
	CellState
}

// ResolvedCellState is a cell's state resolved through its config: the primary
// Process's rhythm plus each sub-Process's rhythm. sub_processes is always a
// (possibly empty) array — a simple cell draws zero satellites with no special
// casing on the frontend.
type ResolvedCellState struct {
	CellID       string         `json:"cell_id"`
	Station      string         `json:"station"`
	DisplayName  string         `json:"display_name"`
	Primary      ProcessState   `json:"primary"`
	SubProcesses []ProcessState `json:"sub_processes"`
}

// ComputeResolvedCellState partitions a cell's events by process_id and derives
// each Process's live state as of now. Each Process's target cycle is estimated
// from its own event stream (no per-Process configured target exists yet); a
// Process with no events in the window resolves to no-data. The primary and
// every configured sub-Process always appear, even with zero events, so the
// tile renders a stable dot set.
func ComputeResolvedCellState(events []PartEvent, cfg CellConfig, now time.Time, th Thresholds) ResolvedCellState {
	byProc := make(map[int64][]PartEvent, len(cfg.SubProcessIDs)+1)
	for _, e := range events {
		byProc[e.ProcessID] = append(byProc[e.ProcessID], e)
	}
	state := func(pid int64) ProcessState {
		ev := byProc[pid]
		return ProcessState{
			ProcessID: pid,
			CellState: ComputeCellState(ev, EstimateTarget(ev), now, th),
		}
	}
	out := ResolvedCellState{
		CellID:       cfg.CellID,
		Station:      cfg.Station,
		DisplayName:  cfg.DisplayName,
		Primary:      state(cfg.PrimaryProcessID),
		SubProcesses: make([]ProcessState, 0, len(cfg.SubProcessIDs)),
	}
	for _, sub := range cfg.SubProcessIDs {
		out.SubProcesses = append(out.SubProcesses, state(sub))
	}
	return out
}

// ProcessWindow is one Process's event history + derived metrics over a window
// — the per-Process pulse timeline the cell drill renders.
type ProcessWindow struct {
	ProcessID int64       `json:"process_id"`
	Primary   bool        `json:"primary"`
	Events    []PartEvent `json:"events"`
	Metrics   CellMetrics `json:"metrics"`
}

// CellHeartbeat is a cell's windowed history split per Process (primary first,
// then subs in config order) — the drill payload (Phase E).
type CellHeartbeat struct {
	CellID      string          `json:"cell_id"`
	Station     string          `json:"station"`
	DisplayName string          `json:"display_name"`
	Since       time.Time       `json:"since"`
	Until       time.Time       `json:"until"`
	Processes   []ProcessWindow `json:"processes"`
}

// ComputeCellHeartbeat partitions a cell's windowed events by process_id and
// derives each Process's pulse history + loss metrics. Pure; the per-Process
// target is estimated from that Process's own stream. Primary leads, subs follow
// in config order; a configured Process with no events yields an empty window.
func ComputeCellHeartbeat(events []PartEvent, cfg CellConfig, since, until time.Time, th Thresholds) CellHeartbeat {
	byProc := make(map[int64][]PartEvent, len(cfg.SubProcessIDs)+1)
	for _, e := range events {
		byProc[e.ProcessID] = append(byProc[e.ProcessID], e)
	}
	window := func(pid int64, primary bool) ProcessWindow {
		ev := byProc[pid]
		if ev == nil {
			ev = []PartEvent{}
		}
		return ProcessWindow{
			ProcessID: pid,
			Primary:   primary,
			Events:    ev,
			Metrics:   ComputeMetrics(ev, since, until, EstimateTarget(ev), th),
		}
	}
	out := CellHeartbeat{
		CellID: cfg.CellID, Station: cfg.Station, DisplayName: cfg.DisplayName,
		Since: since, Until: until,
		Processes: make([]ProcessWindow, 0, len(cfg.SubProcessIDs)+1),
	}
	out.Processes = append(out.Processes, window(cfg.PrimaryProcessID, true))
	for _, sub := range cfg.SubProcessIDs {
		out.Processes = append(out.Processes, window(sub, false))
	}
	return out
}
