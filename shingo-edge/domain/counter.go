package domain

import "time"

// HourlyCount aggregates counter_snapshots deltas into per-hour buckets
// keyed by Process + Style + date + hour. Driven by a periodic roll-up
// from the snapshots table.
// BucketStart is the unix second at the start of the UTC hour this count
// landed in. It is deliberately NOT a plant-local date and hour: storing
// those put a timezone into the data, which made a wrong or unset zone
// permanent instead of cosmetic, and merged two real hours into one row every
// autumn when the local clock repeated 01:00. The plant zone is applied on
// read, where it belongs. See store/schema/sqlite_ddl.go.
type HourlyCount struct {
	ID          int64 `json:"id"`
	ProcessID   int64 `json:"process_id"`
	StyleID     int64 `json:"style_id"`
	BucketStart int64 `json:"bucket_start"`
	Delta       int64 `json:"delta"`
}

// ReportingPoint is one PLC counter binding — the (PLC, tag) pair, the
// Process + Style it scores for, and the last polled count value used
// to compute the next CounterSnapshot delta. WarlinkManaged means the
// counter is driven by an external Warlink integration rather than
// the edge poller.
type ReportingPoint struct {
	ID             int64      `json:"id"`
	PLCName        string     `json:"plc_name"`
	TagName        string     `json:"tag_name"`
	StyleID        int64      `json:"style_id"`
	LastCount      int64      `json:"last_count"`
	LastPollAt     *time.Time `json:"last_poll_at"`
	Enabled        bool       `json:"enabled"`
	WarlinkManaged bool       `json:"warlink_managed"`
	ProcessID      int64      `json:"process_id"`
}
