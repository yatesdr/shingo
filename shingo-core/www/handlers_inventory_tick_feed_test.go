//go:build docker

package www

import (
	"net/http"
	"strings"
	"testing"
)

// TestHandleInventory_TickFeedPanel: the Inventory page carries one row per
// station for the production tick feed, flagging a station whose Edge reports
// an unsent tick older than the lag threshold and one that has never reported
// the lag (an Edge from before the shipper).
func TestHandleInventory_TickFeedPanel(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForPages(t)
	for _, uid := range []string{"stn-feed-behind", "stn-feed-silent"} {
		if _, err := db.IntroduceEdge(uid, "host-"+uid, "v-test"); err != nil {
			t.Fatalf("introduce %s: %v", uid, err)
		}
	}
	if _, err := db.Exec(`UPDATE edge_registry SET display_name = 'Behind Cell',
		tick_pending = 40, tick_oldest_unsent_age_ms = 900000, tick_reported_at = NOW(), tick_rejected = 2
		WHERE station_uid = 'stn-feed-behind'`); err != nil {
		t.Fatalf("seed lag: %v", err)
	}
	if _, err := db.Exec(`UPDATE edge_registry SET display_name = 'Silent Cell' WHERE station_uid = 'stn-feed-silent'`); err != nil {
		t.Fatal(err)
	}

	rec := getPlain(t, h.handleInventory, "/inventory")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="tick-feed"`) {
		t.Fatalf("no tick-feed panel on /inventory")
	}
	i := strings.Index(body, `id="tick-feed"`)
	panel := body[i:]
	if j := strings.Index(panel, "</section>"); j > 0 {
		panel = panel[:j]
	}
	for _, want := range []string{"Behind Cell", "lagging", "ticks rejected", "Silent Cell", "no report"} {
		if !strings.Contains(panel, want) {
			t.Errorf("tick-feed panel missing %q:\n%s", want, panel)
		}
	}
}
