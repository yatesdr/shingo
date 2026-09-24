package service

import (
	"testing"
	"time"

	"shingocore/domain"
)

func i64(v int64) *int64 { return &v }

// TestClassifyTickFeed pins the two flags the Inventory page shows per
// station: LAGGING when the Edge reports an unsent tick older than
// TickFeedLagThreshold, and NO REPORT when the station's last report of the
// lag fields is older than TickFeedReportStaleAfter or never came (an Edge
// from before the shipper, or a heartbeat loop that stopped).
func TestClassifyTickFeed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-30 * time.Second)
	old := now.Add(-TickFeedReportStaleAfter - time.Second)
	edges := []domain.RegistryEdge{
		{StationID: "stn-ok", DisplayName: "OK", TickPending: i64(0), TickOldestUnsentAgeMS: i64(0), TickReportedAt: &fresh},
		{StationID: "stn-behind", DisplayName: "Behind", TickPending: i64(40), TickOldestUnsentAgeMS: i64(TickFeedLagThreshold.Milliseconds() + 1), TickReportedAt: &fresh},
		{StationID: "stn-silent", DisplayName: "Silent", TickPending: i64(0), TickOldestUnsentAgeMS: i64(0), TickReportedAt: &old},
		{StationID: "stn-old-edge", DisplayName: "Old"},
	}
	got := ClassifyTickFeed(edges, now)
	want := map[string][2]bool{ // lagging, reportStale
		"stn-ok":       {false, false},
		"stn-behind":   {true, false},
		"stn-silent":   {false, true},
		"stn-old-edge": {false, true},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %d, want %d", len(got), len(want))
	}
	for _, g := range got {
		w, ok := want[g.StationID]
		if !ok {
			t.Errorf("unexpected station %s", g.StationID)
			continue
		}
		if g.Lagging != w[0] || g.ReportStale != w[1] {
			t.Errorf("%s: lagging/stale = %v/%v, want %v/%v", g.StationID, g.Lagging, g.ReportStale, w[0], w[1])
		}
	}
}
