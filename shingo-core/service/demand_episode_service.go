package service

import (
	"time"

	"shingocore/domain"
	"shingocore/store"
	"shingocore/store/orders"
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

// ChildOutcomesSince returns the raw (episode, status, reached-vendor) tally for
// every episode in the browser's window. Feeds 5.2.
//
// RAW, NOT CLASSIFIED. The categories — consumption, dying orders, cancels
// either side of dispatch — are display rules that change as the protocol
// grows, and they live in one tested pure function in www rather than being
// split between a SQL CASE, a service method and a renderer.
func (s *DemandEpisodeService) ChildOutcomesSince(since time.Time) ([]domain.ChildStatusCount, error) {
	return s.db.CountChildrenByStatus(since)
}

// OrphanTrend returns per-bucket orphan and total order counts since `since`,
// keyed on orders.created_at. Feeds 5.7.
//
// ONLY NON-EMPTY BUCKETS COME BACK — see www.BuildOrphanTrend, which generates
// the full series and renders the gaps as unmeasured rather than as zero.
func (s *DemandEpisodeService) OrphanTrend(since time.Time, bucket time.Duration) ([]domain.OrphanBucket, error) {
	return s.db.OrphanRateBuckets(since, bucket)
}

// OrphanSites returns the per-station orphan lane, worst first. Feeds 5.7.
//
// Bounded by station count, unlike DB.ListOrphanFindings which is deliberately
// unbounded and returns every individual finding. Both are wanted: the lane is
// what a page can render whole, the findings list is the drill-down.
func (s *DemandEpisodeService) OrphanSites() ([]domain.OrphanSite, error) {
	return s.db.SummarizeOrphansBySite()
}

// Get reads one episode's header. Returns (nil, nil) when there is no such
// episode, which the caller must render as 404 rather than as a read failure.
//
// The nil-nil is not a wart; it is the distinction the detail page turns on. "No
// episode with this id" and "the episode store could not be read" are different
// facts and only one of them is a system problem. A signature that returned
// sql.ErrNoRows would make the handler classify a string comparison to tell them
// apart, and the first refactor that wrapped the error would silently reclassify
// every unknown id as an outage.
func (s *DemandEpisodeService) Get(originID string) (*domain.DemandOrigin, error) {
	return s.db.GetDemandOrigin(originID)
}

// Orders returns every order this episode spawned, oldest first, capped at
// limit; the bool reports whether the cap bit.
//
// A SEPARATE CALL FROM Get, AND THAT IS THE POINT. The child read can fail on
// its own, and when it does the page must say "an unknown number of orders",
// never "no orders". Folding the two into one call that returned a single error
// would leave the handler unable to tell a headerless episode from a childless
// one, and the childless rendering — an empty table under a real demand — is the
// single most alarming thing this surface can display. It has to be earned.
func (s *DemandEpisodeService) Orders(originID string, limit int) ([]*domain.Order, bool, error) {
	return orders.ListByOrigin(s.db.DB, originID, limit)
}
