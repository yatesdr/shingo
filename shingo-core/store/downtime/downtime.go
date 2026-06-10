package downtime

import "time"

// DowntimeEvent represents one persisted downtime start or end event (G9).
// Maps to the downtime_events table in core Postgres. Two events per outage:
// one "down" (started) and one "up" (ended).
type DowntimeEvent struct {
	ID          int64
	Station     string
	PLCName     string
	Reason      string
	StartedAt   time.Time
	EndedAt     time.Time
	DurationMS  int64
	EdgeEventID int64
}
