package service

import (
	"shingocore/store"
)

// MissionService centralizes mission telemetry and statistics queries.
// Handlers call MissionService instead of reaching through engine
// passthroughs to *store.DB.
//
// Absorbed from engine_db_methods.go as part of the Phase 3a closeout
// (PR 3a.6). Methods are thin delegates today.
type MissionService struct {
	db *store.DB
}

func NewMissionService(db *store.DB) *MissionService {
	return &MissionService{db: db}
}

// Stats returns summary counters across missions matching the filter.
func (s *MissionService) Stats(f store.MissionFilter) (*store.MissionStats, error) {
	return s.db.GetMissionStats(f)
}

// Telemetry returns the latest telemetry snapshot for a single
// mission (keyed by order ID).
func (s *MissionService) Telemetry(orderID int64) (*store.MissionTelemetry, error) {
	return s.db.GetMissionTelemetry(orderID)
}

// ListEvents returns the event timeline for a single mission.
func (s *MissionService) ListEvents(orderID int64) ([]*store.MissionEvent, error) {
	return s.db.ListMissionEvents(orderID)
}

// List returns telemetry for every mission matching the filter along
// with a total row count (for pagination).
func (s *MissionService) List(f store.MissionFilter) ([]*store.MissionTelemetry, int, error) {
	return s.db.ListMissions(f)
}
