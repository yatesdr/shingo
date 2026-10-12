package engine

import (
	"time"

	"shingo/protocol"
	"shingocore/domain"
)

// forecaster_close.go — a forecast that will not open is closed by the thing
// that wrote it, and the close is the score (D2.2; v4 §2.2; DESIGN-D2 §4).
//
// Only the forecaster closes a forecast. A place that opens is not closed at
// all: the door changes the record's tense and it goes on to settle as a
// demand. The three reasons are Core's own and never cross the wire.

// forecastExpiryGrace is how long past its projected need a forecast may stay
// unopened before it is a miss (F5's companion: provisional, one constant,
// measured on the sim and then ruled).
const forecastExpiryGrace = 10 * time.Minute

// forecastClose decides whether a place's forecast closes this pass, and why:
// expired when its projected need has passed by the grace and it never opened;
// recovered by another hand when its stock rose with no demand open, so the
// projection moved later past the band and is still ahead of now. "" keeps it.
func forecastClose(rec domain.ForecastRecord, cur *time.Time, now time.Time) string {
	if rec.Open || rec.NeedAt == nil {
		return ""
	}
	if now.After(rec.NeedAt.Add(forecastExpiryGrace)) {
		return domain.CloseReasonExpired
	}
	// cur.After(now): a projection of now is a place that is due (its line at
	// or below the level, its supply in flight), not stock that rose.
	if cur != nil && cur.After(now) && cur.After(*rec.NeedAt) && needAtMoved(rec.NeedAt, cur, now) {
		return domain.CloseReasonRecoveredUnattributed
	}
	return ""
}

// closeForecast writes the close, closed_by sweep.
func (m *SourceabilityMonitor) closeForecast(rec domain.ForecastRecord, reason string, now time.Time) {
	if _, err := m.eng.db.CloseForecast(rec.OriginID, reason, protocol.ClosedBySweep, now); err != nil {
		m.eng.logFn("forecast: %s: close %s: %v", rec.EpisodeKey, reason, err)
	}
}

// withdrawForecasts closes every forecast whose place left the population this
// pass (a claim retired, the style changed, a binding removed or zeroed).
func (m *SourceabilityMonitor) withdrawForecasts(records []domain.ForecastRecord, places map[string]bool, now time.Time) {
	for _, r := range records {
		if r.Open || places[r.EpisodeKey] {
			continue
		}
		m.closeForecast(r, domain.CloseReasonWithdrawn, now)
	}
}
