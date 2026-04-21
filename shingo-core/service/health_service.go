package service

import (
	"shingocore/store"
)

// HealthService exposes lightweight liveness checks used by the
// dashboard and diagnostics handlers. Handlers call HealthService
// instead of reaching through engine passthroughs to *store.DB.
//
// Absorbed from engine_db_methods.go as part of the Phase 3a closeout
// (PR 3a.6). Fleet / messaging / count-group health remains on the
// engine (each subsystem owns its own health surface); this service
// only covers the database-side ping.
type HealthService struct {
	db *store.DB
}

func NewHealthService(db *store.DB) *HealthService {
	return &HealthService{db: db}
}

// PingDB returns nil when the database is reachable.
func (s *HealthService) PingDB() error {
	return s.db.Ping()
}
