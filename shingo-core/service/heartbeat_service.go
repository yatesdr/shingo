package service

import (
	"encoding/json"
	"time"

	"shingocore/store"
	"shingocore/store/heartbeat"
)

// HeartbeatService is the read surface for the production-heartbeat dashboards
// (plan §12): it reads cell_part_events and runs the pure analytical core
// (ComputeCellState/Stops/Metrics). Reached via the engine by handlers_cells
// (www-no-direct-store depguard). The ingest/projection side lives in
// messaging.CoreDataService.
type HeartbeatService struct {
	db *store.DB
}

func NewHeartbeatService(db *store.DB) *HeartbeatService {
	return &HeartbeatService{db: db}
}

// Events returns the raw projected ticks for a cell in [since, until].
func (s *HeartbeatService) Events(cellID string, since, until time.Time) ([]heartbeat.PartEvent, error) {
	return s.db.ListCellPartEvents(cellID, since, until)
}

// CellCatalog returns the auto-derived catalog cells (Q-034). station=="" → all
// stations; otherwise that station only. Stale cells are included so the setup
// UI can show + prune retired PLCs.
func (s *HeartbeatService) CellCatalog(station string) ([]store.EdgeCell, error) {
	return s.db.ListEdgeCells(station)
}

// Stops returns the discrete stop events in [since, until]. The target cycle is
// the stream's own median per-part gap (heartbeat.EstimateTarget): nothing
// configures one (cell_targets never had a writer, and v185 drops it).
func (s *HeartbeatService) Stops(cellID string, since, until time.Time) ([]heartbeat.StopEvent, error) {
	events, err := s.db.ListCellPartEvents(cellID, since, until)
	if err != nil {
		return nil, err
	}
	return heartbeat.ComputeStops(events, heartbeat.EstimateTarget(events), heartbeat.DefaultThresholds()), nil
}

// ── cells ───────────────────────────────────────────────────────────────────

// ListCells returns the cells to display: the auto-derived catalog cells
// (Q-034), a cell per PLC an Edge reported, so a freshly registered Edge's
// heartbeat populates with no manual setup. Nothing stores a hand-made cell
// (v189 dropped cell_config); a board renames or hides a cell in its own setup
// (DashboardCells). A catalog-read failure is the error, not an empty list.
func (s *HeartbeatService) ListCells() ([]heartbeat.CellConfig, error) {
	return s.derivedCatalogCells()
}

// DashboardCells resolves the cells a heartbeat dashboard shows: the catalog
// cells (ListCells) scoped to the dashboard's stations, with
// per-dashboard overrides from its config JSON applied (hide a cell, rename it).
// This is the "cell setup lives with the dashboard" path (refactor #4) — the
// kiosk reads it instead of the global /api/cells when rendered as a board.
// Empty stations = whole plant; empty/absent config = show all, default names.
func (s *HeartbeatService) DashboardCells(stations []string, configJSON []byte) ([]heartbeat.CellConfig, error) {
	cells, err := s.ListCells()
	if err != nil {
		return nil, err
	}
	if len(stations) > 0 {
		want := make(map[string]bool, len(stations))
		for _, st := range stations {
			want[st] = true
		}
		kept := make([]heartbeat.CellConfig, 0, len(cells))
		for _, c := range cells {
			if want[c.StationUID] {
				kept = append(kept, c)
			}
		}
		cells = kept
	}

	var cfg struct {
		Cells map[string]struct {
			Hide bool   `json:"hide"`
			Name string `json:"name"`
		} `json:"cells"`
	}
	if len(configJSON) > 0 {
		_ = json.Unmarshal(configJSON, &cfg) // bad config → no overrides, not an error
	}
	out := make([]heartbeat.CellConfig, 0, len(cells))
	for _, c := range cells {
		if ov, ok := cfg.Cells[c.CellLabel]; ok {
			if ov.Hide {
				continue
			}
			if ov.Name != "" {
				c.DisplayName = ov.Name
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// derivedCatalogCells turns the live (non-stale) edge_cells catalog into
// display cells. A catalog-read failure is an error: the catalog is the only
// source of cells (v189 dropped cell_config), so a list read through a failure
// would show "no cells" for an outage.
func (s *HeartbeatService) derivedCatalogCells() ([]heartbeat.CellConfig, error) {
	cells, err := s.db.ListEdgeCells("")
	if err != nil {
		return nil, err
	}
	out := make([]heartbeat.CellConfig, 0, len(cells))
	for _, c := range cells {
		if c.Stale {
			continue // a retired PLC stays in history but isn't shown as a live cell
		}
		if cfg, ok := deriveCellConfig(c); ok {
			out = append(out, cfg)
		}
	}
	return out, nil
}

// deriveCellConfig maps one catalog cell to a CellConfig. cell_id = the PLC
// label (PLC names are plant-unique in practice); the first binding's process
// is the primary pulse, the rest satellites — an operator refines this via the
// per-dashboard setup (refactor #4). Returns false when the cell has no
// process bindings (nothing to pulse).
func deriveCellConfig(c store.EdgeCell) (heartbeat.CellConfig, bool) {
	var bindings []struct {
		ProcessID int64 `json:"process_id"`
	}
	if err := json.Unmarshal(c.Bindings, &bindings); err != nil || len(bindings) == 0 {
		return heartbeat.CellConfig{}, false
	}
	subs := make([]int64, 0, len(bindings)-1)
	for _, b := range bindings[1:] {
		subs = append(subs, b.ProcessID)
	}
	return heartbeat.CellConfig{
		CellLabel:        c.CellLabel,
		StationUID:       c.StationUID,
		PrimaryProcessID: bindings[0].ProcessID,
		SubProcessIDs:    subs,
		DisplayName:      c.CellLabel,
	}, true
}

// resolveCellConfig resolves a cell_id to its grouping for the live heartbeat:
// the auto-derived catalog cell of that label (the first live one, in
// ListEdgeCells' station order); otherwise (false) the caller falls back to
// the station grain. A catalog-read failure falls back too. This is the single
// seam that makes every resolve path (state, heartbeat, stops) catalog-aware
// without duplicating the lookup.
func (s *HeartbeatService) resolveCellConfig(cellID string) (heartbeat.CellConfig, bool) {
	cells, err := s.db.ListEdgeCells("")
	if err != nil {
		return heartbeat.CellConfig{}, false // non-fatal → station-grain fallback
	}
	for _, c := range cells {
		if c.CellLabel == cellID && !c.Stale {
			if dcfg, ok := deriveCellConfig(c); ok {
				return dcfg, true
			}
		}
	}
	return heartbeat.CellConfig{}, false
}

// ResolveCellState resolves a cell_id to its primary/sub-Process split as of
// now. When the id is no live catalog cell it falls back to the Phase B
// station-grain behavior — the whole station stream as a single primary
// (process_id 0) — so the endpoint keeps working for a bare station id.
func (s *HeartbeatService) ResolveCellState(cellID string, now time.Time) (heartbeat.ResolvedCellState, error) {
	cfg, ok := s.resolveCellConfig(cellID)
	if !ok {
		events, err := s.db.ListCellPartEvents(cellID, now.Add(-2*time.Hour), now)
		if err != nil {
			return heartbeat.ResolvedCellState{}, err
		}
		cs := heartbeat.ComputeCellState(events, heartbeat.EstimateTarget(events), now, heartbeat.DefaultThresholds())
		return heartbeat.ResolvedCellState{
			CellID: cellID, Station: cellID, DisplayName: cellID,
			Primary:      heartbeat.ProcessState{ProcessID: 0, CellState: cs},
			SubProcesses: []heartbeat.ProcessState{},
		}, nil
	}
	events, err := s.db.ListCellPartEvents(cfg.StationUID, now.Add(-2*time.Hour), now)
	if err != nil {
		return heartbeat.ResolvedCellState{}, err
	}
	return heartbeat.ComputeResolvedCellState(events, cfg, now, heartbeat.DefaultThresholds()), nil
}

// ResolveCellHeartbeat returns a cell's windowed history split per Process —
// the drill payload (Phase E). An id that is no live catalog cell falls back
// to the station-grain whole stream as a single primary Process (process_id 0).
func (s *HeartbeatService) ResolveCellHeartbeat(cellID string, since, until time.Time) (heartbeat.CellHeartbeat, error) {
	cfg, ok := s.resolveCellConfig(cellID)
	if !ok {
		events, err := s.db.ListCellPartEvents(cellID, since, until)
		if err != nil {
			return heartbeat.CellHeartbeat{}, err
		}
		cfg = heartbeat.CellConfig{CellLabel: cellID, StationUID: cellID, DisplayName: cellID, PrimaryProcessID: 0}
		return heartbeat.ComputeCellHeartbeat(events, cfg, since, until, heartbeat.DefaultThresholds()), nil
	}
	events, err := s.db.ListCellPartEvents(cfg.StationUID, since, until)
	if err != nil {
		return heartbeat.CellHeartbeat{}, err
	}
	return heartbeat.ComputeCellHeartbeat(events, cfg, since, until, heartbeat.DefaultThresholds()), nil
}

// resolveStation maps a cell_id to the station its events land under, falling
// back to treating the id as the station itself (Phase B). Used by the
// station-grain Stops endpoint until it gains a per-Process split.
func (s *HeartbeatService) resolveStation(cellID string) string {
	if cfg, ok := s.resolveCellConfig(cellID); ok {
		return cfg.StationUID
	}
	return cellID
}

// StopsForCell returns the discrete stops for a cell over [since,until] at the
// station grain (cell_id resolved to its station).
func (s *HeartbeatService) StopsForCell(cellID string, since, until time.Time) ([]heartbeat.StopEvent, error) {
	return s.Stops(s.resolveStation(cellID), since, until)
}
