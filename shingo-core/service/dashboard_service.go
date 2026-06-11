package service

import (
	"fmt"
	"strings"

	"shingocore/store"
	"shingocore/store/dashboards"
)

// DashboardService is the floor-display platform's CRUD surface. A dashboard
// is a saved, station-scoped view of Core's live data (the AMR task board
// today; other kinds later). This service is plain CRUD over store/dashboards
// with light input normalization — dashboards own no operational state, so
// there is no cross-aggregate orchestration here.
type DashboardService struct {
	db *store.DB
}

func NewDashboardService(db *store.DB) *DashboardService {
	return &DashboardService{db: db}
}

// DefaultKind is the renderer assigned when an input omits kind.
const DefaultKind = "task-board"

// DashboardInput is the create/update request shape, re-exported so www
// handlers can decode requests through the service layer rather than importing
// the store package directly (the www-no-direct-store depguard guardrail). It
// is a type alias — identical to store/dashboards.Input.
type DashboardInput = dashboards.Input

// List returns all dashboards, ordered for display.
func (s *DashboardService) List() ([]dashboards.Dashboard, error) {
	return dashboards.List(s.db.DB)
}

// Get returns one dashboard by id, or (nil, nil) if it does not exist.
func (s *DashboardService) Get(id int64) (*dashboards.Dashboard, error) {
	return dashboards.Get(s.db.DB, id)
}

// Create validates, normalizes, and inserts a dashboard.
func (s *DashboardService) Create(in dashboards.Input) (int64, error) {
	in, err := normalizeDashboard(in)
	if err != nil {
		return 0, err
	}
	return dashboards.Create(s.db.DB, in)
}

// Update validates, normalizes, and overwrites a dashboard.
func (s *DashboardService) Update(id int64, in dashboards.Input) error {
	in, err := normalizeDashboard(in)
	if err != nil {
		return err
	}
	return dashboards.Update(s.db.DB, id, in)
}

// Delete removes a dashboard by id.
func (s *DashboardService) Delete(id int64) error {
	return dashboards.Delete(s.db.DB, id)
}

// HeartbeatKind is the board renderer for the production-heartbeat kiosk.
const HeartbeatKind = "heartbeat"

// defaultDashboardSeeds is the one-full-plant-board-per-type set seeded out of
// the box (refactor #5). Each is whole-plant (empty station scope); the plant
// heartbeat shows every auto-derived catalog cell, the flight board / robot map
// the whole fleet. Operators drill down from these to make scoped "mini" boards.
var defaultDashboardSeeds = []struct{ kind, name string }{
	{HeartbeatKind, "Plant Heartbeat"},
	{"task-board", "Plant Flight Board"},
	{"robot-map", "Plant Robot Map"},
}

// SeedDefaultDashboards creates one full-plant dashboard per known type for any
// type that has no dashboard yet (refactor #5), so the hub and the floor kiosk
// are useful out of the box. Idempotent and per-kind: a type that already has
// any board (seeded or operator-created) is skipped, so it never clobbers
// curation and is safe on every startup. Returns the number created.
func (s *DashboardService) SeedDefaultDashboards() (int, error) {
	boards, err := dashboards.List(s.db.DB)
	if err != nil {
		return 0, err
	}
	haveKind := make(map[string]bool, len(boards))
	for _, d := range boards {
		haveKind[d.Kind] = true
	}
	seeded := 0
	for i, seed := range defaultDashboardSeeds {
		if haveKind[seed.kind] {
			continue
		}
		if _, err := s.Create(dashboards.Input{
			Name:      seed.name,
			Kind:      seed.kind,
			Stations:  nil, // whole plant
			Enabled:   true,
			SortOrder: i,
		}); err != nil {
			return seeded, err
		}
		seeded++
	}
	return seeded, nil
}

// normalizeDashboard trims and defaults the input. Name is required; kind
// defaults to DefaultKind. Station entries are trimmed, de-duplicated, and
// empties dropped so the stored area filter is clean.
func normalizeDashboard(in dashboards.Input) (dashboards.Input, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return in, fmt.Errorf("dashboard name is required")
	}
	in.Kind = strings.TrimSpace(in.Kind)
	if in.Kind == "" {
		in.Kind = DefaultKind
	}
	clean := make([]string, 0, len(in.Stations))
	seen := map[string]bool{}
	for _, st := range in.Stations {
		st = strings.TrimSpace(st)
		if st == "" || seen[st] {
			continue
		}
		seen[st] = true
		clean = append(clean, st)
	}
	in.Stations = clean
	return in, nil
}
