//go:build docker

package www

import (
	"database/sql"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"shingo/protocol/testutil"
)

// TestFeedsPin_EdgesPageRow pins what one claimed, registered station's row on
// /edges renders: the text of each cell in order.
//
// The Status cell also carries the station's plain-text feed flags from
// EdgeFeedFlags (handlers_edges.go's FeedFlags); this fixture has none, so the
// cell is the status alone. No www test renders a flagged row.
func TestFeedsPin_EdgesPageRow(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForRendering(t)

	const uid = "edge.test"
	_, err := db.EnrollEdge(uid, "", uid)
	testutil.MustNoErr(t, err, "enroll")
	_, err = db.RegisterEdge(uid, "edge-host-a", "inst-a", "test", "America/Chicago", sql.NullInt64{Int64: 3, Valid: true})
	testutil.MustNoErr(t, err, "register")

	body := renderEdgesPage(t, h)
	row := regexp.MustCompile(`(?s)<tr data-uid="edge\.test".*?</tr>`).FindString(body)
	if row == "" {
		t.Fatalf("no row for %s on the page:\n%s", uid, body)
	}

	tags := regexp.MustCompile(`(?s)<[^>]*>`)
	space := regexp.MustCompile(`\s+`)
	var cells []string
	for _, m := range regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`).FindAllStringSubmatch(row, -1) {
		cells = append(cells, strings.TrimSpace(space.ReplaceAllString(tags.ReplaceAllString(m[1], " "), " ")))
	}

	// Name (display name defaults to the uid), uid, host, schema, status,
	// zone, clock, conflicts, action. The Clock cell is the station's clock
	// offset from its last heartbeat: "-" here, where no heartbeat has carried
	// one (TestEdgesPage_FlagsAClockOffsetAboveTheFlushWindow pins a reading).
	want := []string{"edge.test", "edge.test", "edge-host-a", "v3", "active", "America/Chicago", "-", "0", "Rename"}
	if !reflect.DeepEqual(cells, want) {
		t.Errorf("edges row cells = %q, want %q", cells, want)
	}
	if strings.Contains(row, "edge-unclaimed") {
		t.Error("an enrolled station rendered as unclaimed")
	}
}
