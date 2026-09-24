package service

import (
	"time"

	"shingocore/domain"
)

// TickFeedLagThreshold flags a station whose edge reports an unsent production
// tick older than this.
//
// FIVE MINUTES because below it the shipper's own retry covers the cause: a
// failed publish backs off from 1 s to at most 30 s, and a Kafka reconnect
// or a Core restart clears inside a few of those. Past it something is stuck —
// a broker outage, a shipper that stopped, a partition missing at Core — and
// the live tiles are showing stale state. It is also the old feed's TTL, the
// age at which the outbox path started dropping ticks, so a station flagged
// here is one that would have been losing data before the shipper existed.
const TickFeedLagThreshold = 5 * time.Minute

// TickFeedReportStaleAfter flags a station whose last lag report is older than
// this, or which never sent one. Edges heartbeat every 60 s (the heartbeat
// carries the report), so three missed heartbeats — the same margin the
// heartbeat's own 90 s TTL gives one late one, plus two more.
const TickFeedReportStaleAfter = 3 * time.Minute

// ClassifyTickFeed derives the Inventory page's tick-feed rows from the
// registry, as of now. Pure.
func ClassifyTickFeed(edges []domain.RegistryEdge, now time.Time) []domain.TickFeedStation {
	out := make([]domain.TickFeedStation, 0, len(edges))
	for _, e := range edges {
		row := domain.TickFeedStation{
			StationID:         e.StationID,
			DisplayName:       e.DisplayName,
			Pending:           e.TickPending,
			OldestUnsentAgeMS: e.TickOldestUnsentAgeMS,
			ReportedAt:        e.TickReportedAt,
			Rejected:          e.TickRejected,
		}
		row.ReportStale = e.TickReportedAt == nil || now.Sub(*e.TickReportedAt) > TickFeedReportStaleAfter
		row.Lagging = e.TickPending != nil && *e.TickPending > 0 &&
			e.TickOldestUnsentAgeMS != nil && *e.TickOldestUnsentAgeMS > TickFeedLagThreshold.Milliseconds()
		out = append(out, row)
	}
	return out
}

// TickFeedStatus is the Inventory page's production tick feed panel: one row
// per registered station. One registry read.
func (s *HeartbeatService) TickFeedStatus(now time.Time) ([]domain.TickFeedStation, error) {
	edges, err := s.db.ListEdges()
	if err != nil {
		return nil, err
	}
	return ClassifyTickFeed(edges, now), nil
}
