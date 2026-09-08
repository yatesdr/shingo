package engine

import (
	"log"
	"time"

	"shingoedge/store"
	"shingoedge/store/counters"
)

// HourlyTracker accumulates counter deltas into hourly buckets in the database.
//
// It holds NO location. Buckets are UTC, so the writer has no timezone
// decision to make and cannot drift from the reader — which is what the two
// copies of a zone parse used to risk.
type HourlyTracker struct {
	db *store.DB
}

// ReportingLocation resolves the plant's IANA zone (e.g. "America/Chicago") —
// the zone a stored UTC bucket is REPORTED in. It no longer decides how
// anything is stored: buckets are UTC, so this is a display and roll-up
// concern only, and a wrong value is cosmetic and fixed by restarting with the
// right one rather than by repairing data.
//
// EMPTY FALLS BACK TO UTC, matching www.resolvePlantLocation. It used to fall
// back to time.Local, so the pair DISAGREED on an unconfigured box: display
// said UTC while bucketing said whatever zone the machine happened to sit in.
// At Hopkinsville that was America/Indiana/Indianapolis at a Central plant,
// and it mislabelled three months of hourly counts without ever looking wrong.
// The old note here argued the fallback had to stay time.Local so that
// changing it would not re-zone live history mid-stream — true while the
// history was zoned, and moot now that it is not.
//
// The log names the source, so "which knob made this clock" stays answerable
// from journald.
func ReportingLocation(timezone string) *time.Location {
	if timezone == "" {
		log.Printf("reporting zone: timezone unset in shingoedge.yaml — using UTC. " +
			"Set timezone: to the PLANT zone (the box's OS zone is not it)")
		return time.UTC
	}
	parsed, err := time.LoadLocation(timezone)
	if err != nil {
		log.Printf("reporting zone: invalid timezone %q, using UTC: %v", timezone, err)
		return time.UTC
	}
	return parsed
}

// NewHourlyTracker creates a new HourlyTracker. It takes no timezone, because
// the buckets it writes are UTC.
func NewHourlyTracker(db *store.DB) *HourlyTracker {
	return &HourlyTracker{db: db}
}

// HandleDelta records a counter delta into the current UTC hour bucket.
// Reset anomaly deltas are skipped to avoid counting PLC reset artifacts as production.
func (ht *HourlyTracker) HandleDelta(delta CounterDeltaEvent) {
	if delta.ProcessID == 0 || delta.StyleID == 0 {
		return
	}
	if delta.Anomaly == "reset" {
		return // skip reset-derived deltas
	}

	bucket := counters.HourBucket(time.Now())

	if err := ht.db.UpsertHourlyCount(delta.ProcessID, delta.StyleID, bucket, delta.Delta); err != nil {
		log.Printf("hourly tracker upsert: %v", err)
	}
}
