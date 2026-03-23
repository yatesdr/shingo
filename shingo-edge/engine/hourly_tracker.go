package engine

import (
	"log"
	"time"

	"shingoedge/store"
)

// HourlyTracker accumulates counter deltas into hourly buckets in the database.
type HourlyTracker struct {
	db  *store.DB
	loc *time.Location
}

// NewHourlyTracker creates a new HourlyTracker.
// If timezone is a valid IANA location (e.g. "America/Chicago"), it is used
// for date/hour bucketing. Otherwise the server's local timezone is used.
func NewHourlyTracker(db *store.DB, timezone string) *HourlyTracker {
	loc := time.Local
	if timezone != "" {
		if parsed, err := time.LoadLocation(timezone); err != nil {
			log.Printf("hourly tracker: invalid timezone %q, using local: %v", timezone, err)
		} else {
			loc = parsed
			log.Printf("hourly tracker: using timezone %s", loc)
		}
	}
	return &HourlyTracker{db: db, loc: loc}
}

// HandleDelta records a counter delta into the current date/hour bucket.
// Reset anomaly deltas are skipped to avoid counting PLC reset artifacts as production.
func (ht *HourlyTracker) HandleDelta(delta CounterDeltaEvent) {
	if delta.ProcessID == 0 || delta.StyleID == 0 {
		return
	}
	if delta.Anomaly == "reset" {
		return // skip reset-derived deltas
	}

	now := time.Now().In(ht.loc)
	countDate := now.Format("2006-01-02")
	hour := now.Hour()

	if err := ht.db.UpsertHourlyCount(delta.ProcessID, delta.StyleID, countDate, hour, delta.Delta); err != nil {
		log.Printf("hourly tracker upsert: %v", err)
	}
}
