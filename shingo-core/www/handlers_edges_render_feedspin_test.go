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
// /edges renders today: the text of each cell in order.
//
// after (F1): a row may also carry plain-text feed flags from
// EdgeFeedFlags(station) — keys sent three times running, process conflicts.
// With no flags, which is this fixture: same, cell for cell.
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
	// zone, conflicts, action.
	want := []string{"edge.test", "edge.test", "edge-host-a", "v3", "active", "America/Chicago", "0", "Rename"}
	after := want // F1: same when EdgeFeedFlags(uid) is empty
	if !reflect.DeepEqual(cells, want) {
		t.Errorf("edges row cells = %q, want %q (after F1 with no flags: %q)", cells, want, after)
	}
	if strings.Contains(row, "edge-unclaimed") {
		t.Error("an enrolled station rendered as unclaimed (after: same)")
	}
}
