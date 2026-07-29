package service

import (
	"time"

	"shingocore/domain"
	"shingocore/store"
)

// DemandEpisodeService reads the demand grain for the Phase 6 surfaces.
//
// SEPARATE FROM DemandService, which is the production-quota CRUD behind
// /demand — a different concept that happens to share a word. Folding episodes
// into it would put two unrelated aggregates behind one accessor and make the
// nav label "Demand" mean two things at two scales, which is the overloading
// docs/ui-style-guide.md already flags as the worst in this codebase.
//
// Read-only, and it stays that way. Episodes are authored by Edge's emitter and
// by Core's threshold monitor and reconciler; a write path from the web layer
// would be a third author for a row whose whole design is state transfer under a
// revision guard.
type DemandEpisodeService struct {
	db *store.DB
}

func NewDemandEpisodeService(db *store.DB) *DemandEpisodeService {
	return &DemandEpisodeService{db: db}
}

// List returns episodes for the browser: everything open, plus everything that
// closed since `since`, newest-first, capped at `limit`.
//
// The bool reports whether the cap bit. It is returned rather than logged
// because the PAGE has to say so — a truncated list rendered as though it were
// complete is a page that lies about what the floor is doing.
func (s *DemandEpisodeService) List(since time.Time, limit int) ([]domain.DemandEpisode, bool, error) {
	return s.db.ListDemandEpisodes(since, limit)
}

// ClosedBySince returns the raw closed_by of every episode closed since
// `since`, NULL preserved as "". Feeds the 5.6 summary, which counts that third
// state on its own.
func (s *DemandEpisodeService) ClosedBySince(since time.Time) ([]string, error) {
	return s.db.ListClosedBySince(since)
}
