package domain

import "time"

// CounterSnapshot is one row in the counter_snapshots table — a single
// reading from a PLC counter tag, with the delta over the previous
// reading so downstream code doesn't have to recompute it. Renamed
// from `counters.Snapshot` during Stage 2A.2 lift to disambiguate
// from any future "Snapshot" types.
type CounterSnapshot struct {
	ID                int64     `json:"id"`
	ReportingPointID  int64     `json:"reporting_point_id"`
	CountValue        int64     `json:"count_value"`
	Delta             int64     `json:"delta"`
	Anomaly           *string   `json:"anomaly"`
	OperatorConfirmed bool      `json:"operator_confirmed"`
	RecordedAt        time.Time `json:"recorded_at"`
}

// HourlyCount aggregates CounterSnapshot deltas into per-hour buckets
// keyed by Process + Style + date + hour. Driven by a periodic roll-up
// from the snapshots table.
type HourlyCount struct {
	ID        int64  `json:"id"`
	ProcessID int64  `json:"process_id"`
	StyleID   int64  `json:"style_id"`
	CountDate string `json:"count_date"`
	Hour      int    `json:"hour"`
	Delta     int64  `json:"delta"`
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
