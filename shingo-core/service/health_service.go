package service

import (
	"database/sql"
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

// PoolStats returns the database connection-pool counters behind the Core
// Health strip's pool meter.
//
// Nothing in shingo-core read sql.DBStats before this: Core recorded every
// EDGE's health and could not state its own. WaitCount is the one that
// matters — a non-zero wait means a request queued for a connection, which is
// the pool being the bottleneck rather than the database.
//
// ok is false when the DB handle is absent (test fixtures), so the strip can
// render "unavailable" instead of an all-zero pool that looks idle.
func (s *HealthService) PoolStats() (sql.DBStats, bool) {
	if s.db == nil || s.db.DB == nil {
		return sql.DBStats{}, false
	}
	return s.db.DB.Stats(), true
}
