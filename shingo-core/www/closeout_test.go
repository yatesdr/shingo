//go:build docker

package www

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// closeout_test.go — owner ruling 2c of the memory close-out
// (2026-09-24): a report_divergence episode is an inventory count anomaly, shown
// on Core's homepage as well as on the Inventory page.

// seedCountAnomaly opens one report_divergence episode the way the lineside
// comparison records it: a count class naming a carrier at a seat, with both
// sides in the detail. Returns the carrier's label.
func seedCountAnomaly(t *testing.T, db *store.DB) string {
	t.Helper()
	seat := &nodes.Node{Name: "ALN_HOME_ANOM", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(seat), "create seat")
	b := testdb.CreateBinAtNode(t, db, "PART-A", seat.ID, "BIN-HOME-ANOM")
	_, err := db.Exec(`INSERT INTO bin_uop_exception (kind, bin_id, payload_code, actor, occurred_at, op, detail)
		VALUES ('report_divergence', $1, 'PART-A', 'stn-home', NOW() - interval '90 minutes', 'count',
		        '{"key":"count|ALN_HOME_ANOM|PART-A|x","node":"ALN_HOME_ANOM","edge_count":10,"core_count":150}'::jsonb)`, b.ID)
	testutil.MustNoErr(t, err, "open a count anomaly")
	return b.Label
}

// AN OPEN COUNT ANOMALY IS ON THE HOMEPAGE, in the health strip beside the
// other anomaly tile: the carrier, the seat, both counts, and how long it has
// been open. Inverts the pin that the homepage did not know it existed.
func TestCloseout_2c_HomepageNamesTheCountAnomaly(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForPages(t)
	label := seedCountAnomaly(t, db)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.handleDashboard(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{label, "ALN_HOME_ANOM", "Edge <b>10</b>", "Core <b>150</b>", "open 1h 30m", "Count anomalies"} {
		if !strings.Contains(body, want) {
			t.Errorf("the homepage does not show %q", want)
		}
	}
}
