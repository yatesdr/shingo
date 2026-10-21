package www

import (
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/engine"
)

// catalogFeedClock is what the catalog stamp asks of the engine: the feed
// book's times for the catalog copy and whether Core speaks feeds. Asked by
// assertion, as the containment release asks holdMarker, so a handler test's stub
// engine stamps nothing.
type catalogFeedClock interface {
	FeedTimes(key string) (receivedAt, confirmedAt time.Time)
	CoreSpeaksFeeds() bool
}

// stampStationCatalog sets view.Catalog from the held catalog copy's times:
// "as of" when the copy has not been current (the later of its arrival and
// Core's last confirmation) for longer than engine.FeedAsOfAfter, the rule
// bins and containment apply; known false when Core speaks feeds and no copy
// is held at all, containment's rule. Memory only: no statement. The crossing
// of the threshold is announced by the feed book (engine feeds.go
// crossingsLocked), since the board does not poll.
func stampStationCatalog(eng any, view *domain.OperatorStationView, now time.Time) {
	fc, ok := eng.(catalogFeedClock)
	if !ok {
		return
	}
	received, confirmed := fc.FeedTimes(protocol.FeedCatalog)
	if received.IsZero() && confirmed.IsZero() {
		if fc.CoreSpeaksFeeds() {
			view.Catalog = &domain.CatalogKnown{Known: false}
		}
		return
	}
	current, old := engine.CurrentAt(received, confirmed, now)
	if !old {
		return
	}
	at := current.UTC()
	view.Catalog = &domain.CatalogKnown{Known: true, AsOf: &at}
}
