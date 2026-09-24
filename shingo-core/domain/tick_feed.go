package domain

import "time"

// TickFeedStation is one station's row in the Inventory page's production
// tick feed panel: the lag its edge last reported on a heartbeat, and the two
// flags Core derives from it.
type TickFeedStation struct {
	StationID   string `json:"station_id"`
	DisplayName string `json:"display_name"`
	// Pending / OldestUnsentAgeMS / ReportedAt are nil when the station has
	// never reported (an edge from before the shipper).
	Pending           *int64     `json:"pending"`
	OldestUnsentAgeMS *int64     `json:"oldest_unsent_age_ms"`
	ReportedAt        *time.Time `json:"reported_at"`
	// Rejected counts ticks Core received from this station and could not
	// store (edge_registry.tick_rejected). Non-zero means ticks were lost.
	Rejected int64 `json:"rejected"`
	// Lagging: the edge reports an unsent tick older than the lag threshold.
	Lagging bool `json:"lagging"`
	// ReportStale: no report within the staleness window, or none ever — the
	// lag above cannot be trusted as current.
	ReportStale bool `json:"report_stale"`
}

// OldestUnsentAge renders OldestUnsentAgeMS for the page, to the second; ""
// when nothing was reported.
func (t TickFeedStation) OldestUnsentAge() string {
	if t.OldestUnsentAgeMS == nil {
		return ""
	}
	return (time.Duration(*t.OldestUnsentAgeMS) * time.Millisecond).Round(time.Second).String()
}
