package domain

import "time"

// DemandTense is where one demand record stands: forecast, open or settled.
//
// It is derived from the record's two timestamps and never stored, the same
// derivation the SQL fragments in store/internal/helpers/demand_tense.go spell
// for queries. A forecast has not opened (no OpenedAt: the store reads a NULL
// opened_at as the zero time); an open record has opened and not closed; a
// settled record has closed, whether or not its demand ever opened.
type DemandTense string

const (
	DemandForecast DemandTense = "forecast"
	DemandOpen     DemandTense = "open"
	DemandSettled  DemandTense = "settled"
)

// DemandTenseOf derives the tense from a record's two timestamps. Every Go site
// that decides a demand record's tense goes through it (or DemandOrigin.Tense),
// so "not closed" is never read as "open".
func DemandTenseOf(openedAt time.Time, closedAt *time.Time) DemandTense {
	switch {
	case closedAt != nil:
		return DemandSettled
	case openedAt.IsZero():
		return DemandForecast
	default:
		return DemandOpen
	}
}

// Tense is the record's tense. See DemandTenseOf.
func (o DemandOrigin) Tense() DemandTense {
	return DemandTenseOf(o.OpenedAt, o.ClosedAt)
}
