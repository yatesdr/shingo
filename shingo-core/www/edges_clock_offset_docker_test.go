//go:build docker

package www

import (
	"database/sql"
	"html"
	"regexp"
	"strings"
	"testing"

	"shingo/protocol/testutil"
)

// TestEdgesPage_FlagsAClockOffsetAboveTheFlushWindow: the Edges page shows
// each station's clock offset (from its last heartbeat) and flags one whose
// size is above the Edge's flush window (5 s), the granularity the lift's time
// rule judges a boundary tick at. A station with no reading shows none.
func TestEdgesPage_FlagsAClockOffsetAboveTheFlushWindow(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForRendering(t)
	for _, uid := range []string{"stn-skewed", "stn-on-time", "stn-unread"} {
		testutil.MustNoErr(t, enrollAndRegister(db, uid, "host-"+uid, sql.NullInt64{}), "seed "+uid)
	}
	for uid, ms := range map[string]int64{"stn-skewed": -12300, "stn-on-time": 300} {
		_, err := db.DB.Exec(`UPDATE edge_registry SET clock_offset_ms = $2 WHERE station_uid = $1`, uid, ms)
		testutil.MustNoErr(t, err, "store the offset for "+uid)
	}

	body := renderEdgesPage(t, h)
	row := func(uid string) string {
		r := regexp.MustCompile(`(?s)<tr data-uid="` + uid + `".*?</tr>`).FindString(body)
		if r == "" {
			t.Fatalf("no row for %s", uid)
		}
		return html.UnescapeString(r) // the template writes "+" as &#43;
	}
	skewed, onTime, unread := row("stn-skewed"), row("stn-on-time"), row("stn-unread")
	if !strings.Contains(skewed, "-12.3 s") || !strings.Contains(skewed, "edge-clock-flag") {
		t.Errorf("a station 12.3 s behind is not shown and flagged:\n%s", skewed)
	}
	if !strings.Contains(onTime, "+0.3 s") || strings.Contains(onTime, "edge-clock-flag") {
		t.Errorf("a station 0.3 s ahead should show its offset unflagged:\n%s", onTime)
	}
	if strings.Contains(unread, "edge-clock-flag") || strings.Contains(unread, " s</") {
		t.Errorf("a station with no reading shows an offset:\n%s", unread)
	}
	if !strings.Contains(body, ">Clock</th>") {
		t.Error("the table has no Clock column")
	}
}
