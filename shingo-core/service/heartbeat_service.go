package service

import (
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

// ListCells returns every configured cell.
func (s *HeartbeatService) ListCells() ([]heartbeat.CellConfig, error) {
	return s.db.ListCellConfigs()
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
	cfg, ok, err := s.db.GetCellConfig(cellID)
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
	cfg, ok, err := s.db.GetCellConfig(cellID)
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
	if cfg, ok, err := s.db.GetCellConfig(cellID); err == nil && ok {
		return cfg.Station
	}
	return cellID
}

// StopsForCell returns the discrete stops for a cell over [since,until] at the
// station grain (cell_id resolved to its station).
func (s *HeartbeatService) StopsForCell(cellID string, since, until time.Time) ([]heartbeat.StopEvent, error) {
	return s.Stops(s.resolveStation(cellID), since, until)
}
