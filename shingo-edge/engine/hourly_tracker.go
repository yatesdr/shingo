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

// BucketLocation resolves the IANA timezone (e.g. "America/Chicago") that
// hourly_counts.count_date and .hour are bucketed in, falling back to the
// server's local zone when it is empty or unparseable.
//
// EXPORTED SO THE RETENTION PASS CANNOT DISAGREE WITH THE WRITER. This is the
// only place a count_date is derived from, and counters.HourlyRetention's
// cutoff has to be rendered in the same zone or the window is off by a
// calendar day for however many hours the plant sits behind UTC — five, at
// Springfield. Two copies of this parse is exactly how that drift starts, so
// cmd/shingoedge/main.go calls this rather than repeating it.
func BucketLocation(timezone string) *time.Location {
	if timezone == "" {
		return time.Local
	}
	parsed, err := time.LoadLocation(timezone)
	if err != nil {
		log.Printf("hourly bucketing: invalid timezone %q, using local: %v", timezone, err)
		return time.Local
	}
	return parsed
}

// NewHourlyTracker creates a new HourlyTracker.
// If timezone is a valid IANA location (e.g. "America/Chicago"), it is used
// for date/hour bucketing. Otherwise the server's local timezone is used.
func NewHourlyTracker(db *store.DB, timezone string) *HourlyTracker {
	loc := BucketLocation(timezone)
	log.Printf("hourly tracker: using timezone %s", loc)
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
