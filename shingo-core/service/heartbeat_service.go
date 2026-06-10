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

// State returns the live cell state as of now. Target cycle comes from
// cell_targets, falling back to a rolling median estimate over the last 2h
// (§8 #11).
func (s *HeartbeatService) State(cellID string, now time.Time) (heartbeat.CellState, error) {
	events, err := s.db.ListCellPartEvents(cellID, now.Add(-2*time.Hour), now)
	if err != nil {
		return heartbeat.CellState{}, err
	}
	target := s.resolveTarget(cellID, events)
	return heartbeat.ComputeCellState(events, target, now, heartbeat.DefaultThresholds()), nil
}

// Stops returns the discrete stop events in [since, until].
func (s *HeartbeatService) Stops(cellID string, since, until time.Time) ([]heartbeat.StopEvent, error) {
	events, err := s.db.ListCellPartEvents(cellID, since, until)
	if err != nil {
		return nil, err
	}
	return heartbeat.ComputeStops(events, s.resolveTarget(cellID, events), heartbeat.DefaultThresholds()), nil
}

// Metrics returns the aggregate loss/MTBF numbers for [since, until].
func (s *HeartbeatService) Metrics(cellID string, since, until time.Time) (heartbeat.CellMetrics, error) {
	events, err := s.db.ListCellPartEvents(cellID, since, until)
	if err != nil {
		return heartbeat.CellMetrics{}, err
	}
	return heartbeat.ComputeMetrics(events, since, until, s.resolveTarget(cellID, events), heartbeat.DefaultThresholds()), nil
}

// resolveTarget prefers the configured cell target, else a rolling estimate
// from the supplied events.
func (s *HeartbeatService) resolveTarget(cellID string, events []heartbeat.PartEvent) time.Duration {
	if t, ok := s.db.GetCellTarget(cellID, ""); ok {
		return t
	}
	return heartbeat.EstimateTarget(events)
}

// ── cell_config (Phase E, Q-025) ────────────────────────────────────────────

// ListCells returns the cells to display: the auto-derived catalog cells
// (Q-034 — a cell per PLC an edge reported) UNION the operator-configured cells,
// with a configured cell overriding the derived one of the same id (curation
// wins). This is why a freshly-registered edge's heartbeat populates with zero
// manual setup; cell_config is now only an optional override.
func (s *HeartbeatService) ListCells() ([]heartbeat.CellConfig, error) {
	configured, err := s.db.ListCellConfigs()
	if err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(configured))
	for _, c := range configured {
		have[c.CellID] = true
	}
	out := configured
	for _, d := range s.derivedCatalogCells() {
		if !have[d.CellID] {
			out = append(out, d)
		}
	}
	return out, nil
}

// DashboardCells resolves the cells a heartbeat dashboard shows: the full cell
// set (catalog-derived + configured) scoped to the dashboard's stations, with
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
			if want[c.Station] {
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
		if ov, ok := cfg.Cells[c.CellID]; ok {
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
// display cells. A catalog-read failure is swallowed to []: the heartbeat
// degrades to whatever cell_config exists rather than erroring.
func (s *HeartbeatService) derivedCatalogCells() []heartbeat.CellConfig {
	cells, err := s.db.ListEdgeCells("")
	if err != nil {
		return nil
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
	return out
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
		CellID:           c.CellLabel,
		Station:          c.Station,
		PrimaryProcessID: bindings[0].ProcessID,
		SubProcessIDs:    subs,
		DisplayName:      c.CellLabel,
	}, true
}

// resolveCellConfig resolves a cell_id to its grouping for the live heartbeat:
// an operator cell_config wins; otherwise the auto-derived catalog cell of the
// same label; otherwise (false) the caller falls back to the station grain.
// This is the single seam that makes every resolve path (state, heartbeat,
// stops) catalog-aware without duplicating the lookup.
func (s *HeartbeatService) resolveCellConfig(cellID string) (heartbeat.CellConfig, bool, error) {
	cfg, ok, err := s.db.GetCellConfig(cellID)
	if err != nil {
		return heartbeat.CellConfig{}, false, err
	}
	if ok {
		return cfg, true, nil
	}
	cells, err := s.db.ListEdgeCells("")
	if err != nil {
		return heartbeat.CellConfig{}, false, nil // non-fatal → station-grain fallback
	}
	for _, c := range cells {
		if c.CellLabel == cellID && !c.Stale {
			if dcfg, ok := deriveCellConfig(c); ok {
				return dcfg, true, nil
			}
		}
	}
	return heartbeat.CellConfig{}, false, nil
}

// GetCell returns one cell by id; ok is false when it isn't configured.
func (s *HeartbeatService) GetCell(cellID string) (heartbeat.CellConfig, bool, error) {
	return s.db.GetCellConfig(cellID)
}

// UpsertCell creates or updates a cell. Takes primitives (not a heartbeat
// type) so www handlers can call it without importing the store sub-package
// (depguard www-no-direct-store).
func (s *HeartbeatService) UpsertCell(cellID, station string, primaryProcessID int64, subProcessIDs []int64, displayName string) error {
	if subProcessIDs == nil {
		subProcessIDs = []int64{}
	}
	return s.db.UpsertCellConfig(heartbeat.CellConfig{
		CellID:           cellID,
		Station:          station,
		PrimaryProcessID: primaryProcessID,
		SubProcessIDs:    subProcessIDs,
		DisplayName:      displayName,
	})
}

// DeleteCell removes a cell.
func (s *HeartbeatService) DeleteCell(cellID string) error {
	return s.db.DeleteCellConfig(cellID)
}

// CellProcesses lists the Processes ticking for a station, for the admin picker.
func (s *HeartbeatService) CellProcesses(station string) ([]heartbeat.ProcessOption, error) {
	return s.db.DistinctCellProcesses(station)
}

// ResolveCellState resolves a cell_id to its primary/sub-Process split as of
// now. When the id has no cell_config row it falls back to the Phase B
// station-grain behavior — the whole station stream as a single primary
// (process_id 0) — so the endpoint keeps working for an unconfigured station.
func (s *HeartbeatService) ResolveCellState(cellID string, now time.Time) (heartbeat.ResolvedCellState, error) {
	cfg, ok, err := s.resolveCellConfig(cellID)
	if err != nil {
		return heartbeat.ResolvedCellState{}, err
	}
	if !ok {
		events, err := s.db.ListCellPartEvents(cellID, now.Add(-2*time.Hour), now)
		if err != nil {
			return heartbeat.ResolvedCellState{}, err
		}
		cs := heartbeat.ComputeCellState(events, s.resolveTarget(cellID, events), now, heartbeat.DefaultThresholds())
		return heartbeat.ResolvedCellState{
			CellID: cellID, Station: cellID, DisplayName: cellID,
			Primary:      heartbeat.ProcessState{ProcessID: 0, CellState: cs},
			SubProcesses: []heartbeat.ProcessState{},
		}, nil
	}
	events, err := s.db.ListCellPartEvents(cfg.Station, now.Add(-2*time.Hour), now)
	if err != nil {
		return heartbeat.ResolvedCellState{}, err
	}
	return heartbeat.ComputeResolvedCellState(events, cfg, now, heartbeat.DefaultThresholds()), nil
}

// ResolveCellHeartbeat returns a cell's windowed history split per Process —
// the drill payload (Phase E). Unconfigured ids fall back to the station-grain
// whole stream as a single primary Process (process_id 0).
func (s *HeartbeatService) ResolveCellHeartbeat(cellID string, since, until time.Time) (heartbeat.CellHeartbeat, error) {
	cfg, ok, err := s.resolveCellConfig(cellID)
	if err != nil {
		return heartbeat.CellHeartbeat{}, err
	}
	if !ok {
		events, err := s.db.ListCellPartEvents(cellID, since, until)
		if err != nil {
			return heartbeat.CellHeartbeat{}, err
		}
		cfg = heartbeat.CellConfig{CellID: cellID, Station: cellID, DisplayName: cellID, PrimaryProcessID: 0}
		return heartbeat.ComputeCellHeartbeat(events, cfg, since, until, heartbeat.DefaultThresholds()), nil
	}
	events, err := s.db.ListCellPartEvents(cfg.Station, since, until)
	if err != nil {
		return heartbeat.CellHeartbeat{}, err
	}
	return heartbeat.ComputeCellHeartbeat(events, cfg, since, until, heartbeat.DefaultThresholds()), nil
}

// resolveStation maps a cell_id to the station its events land under, falling
// back to treating the id as the station itself (Phase B). Used by the
// station-grain Stops endpoint until it gains a per-Process split.
func (s *HeartbeatService) resolveStation(cellID string) string {
	if cfg, ok, err := s.resolveCellConfig(cellID); err == nil && ok {
		return cfg.Station
	}
	return cellID
}

// StopsForCell returns the discrete stops for a cell over [since,until] at the
// station grain (cell_id resolved to its station).
func (s *HeartbeatService) StopsForCell(cellID string, since, until time.Time) ([]heartbeat.StopEvent, error) {
	return s.Stops(s.resolveStation(cellID), since, until)
}
