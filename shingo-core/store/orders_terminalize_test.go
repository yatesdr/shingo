//go:build docker

package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// TestTerminalizeOrder_AnomalyAndDetailByStatus pins two INDEPENDENT invariants
// of the terminal chokepoint:
//
//   - error_detail is terminal-type-dependent: suppressed for the clean success
//     'confirmed', persisted (the failure/skip reason) for every other terminal.
//   - the _TRANSIT anomaly stamp is NOT terminal-type-dependent: a bin still
//     claimed by the order and parked at _TRANSIT at terminal time never arrived,
//     so it is stamped anomalous for EVERY terminal — including confirmed, whose
//     delivery-arrival can fail and leave the bin stranded (the completion
//     safety-net can't recover it once this chokepoint clears claimed_by). A bin
//     that reached a real node is never stamped, whatever the terminal (the
//     stamp matches zero rows on the happy path).
func TestTerminalizeOrder_AnomalyAndDetailByStatus(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// _TRANSIT is seeded by the schema; the anomaly stamp filters bins parked there.
	var transitID int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT id FROM nodes WHERE name='_TRANSIT'`).Scan(&transitID), "lookup _TRANSIT")
	// A real (non-_TRANSIT) node standing in for a delivery destination.
	dest := &nodes.Node{Name: "DEST-SLOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	// terminalize parks a freshly-claimed bin at parkNodeID under a new order,
	// drives that order to the given terminal, and returns the resulting
	// error_detail and whether the bin's anomaly_at was stamped.
	seq := 0
	terminalize := func(t *testing.T, status protocol.Status, detail string, parkNodeID int64) (sql.NullString, bool) {
		t.Helper()
		seq++
		order := testdb.CreateOrder(t, db)
		bin := testdb.CreateBinAtNode(t, db, "PART-A", parkNodeID, fmt.Sprintf("BIN-%s-%d", status, seq))
		testdb.ClaimBinForTest(t, db, bin.ID, order.ID)
		testutil.MustNoErr(t, db.TerminalizeOrder(order.ID, status, detail), "TerminalizeOrder "+string(status))

		var gotStatus string
		var errDetail sql.NullString
		testutil.MustNoErr(t, db.DB.QueryRow(`SELECT status, error_detail FROM orders WHERE id=$1`, order.ID).Scan(&gotStatus, &errDetail), "read order")
		if gotStatus != string(status) {
			t.Fatalf("status = %q, want %q", gotStatus, status)
		}
		var anomalyAt sql.NullTime
		testutil.MustNoErr(t, db.DB.QueryRow(`SELECT anomaly_at FROM bins WHERE id=$1`, bin.ID).Scan(&anomalyAt), "read bin anomaly")
		return errDetail, anomalyAt.Valid
	}

	// error_detail by terminal — bin parked at a REAL dest node, so the anomaly
	// stamp matches zero rows and each case also proves a delivered bin is never
	// flagged, whatever the terminal.
	t.Run("error_detail (bin at real node, never anomalous)", func(t *testing.T) {
		cases := []struct {
			status protocol.Status
			detail string
			want   string
		}{
			{protocol.StatusFailed, "fleet rejected order", "fleet rejected order"},
			{protocol.StatusCancelled, "operator cancelled", "operator cancelled"},
			{protocol.StatusSkipped, "no bin at any pickup node", "no bin at any pickup node"},
			{protocol.StatusConfirmed, "delivered 5/5", ""}, // suppressed for the clean success
		}
		for _, c := range cases {
			t.Run(string(c.status), func(t *testing.T) {
				d, anom := terminalize(t, c.status, c.detail, dest.ID)
				if d.String != c.want {
					t.Errorf("error_detail = %q, want %q", d.String, c.want)
				}
				if anom {
					t.Errorf("bin at real node stamped anomalous on %s, want NOT stamped", c.status)
				}
			})
		}
	})

	// _TRANSIT-stranded bin — anomalous for EVERY terminal. This is the confirmed
	// regression fix: pre-Option-1 only the failure terminals stamped, so a
	// confirmed order whose arrival failed left its bin at _TRANSIT untimestamped
	// (buried at the bottom of the operator anomalies list, no "released
	// mid-flight" signal).
	t.Run("stranded _TRANSIT bin is anomalous for every terminal", func(t *testing.T) {
		for _, status := range []protocol.Status{
			protocol.StatusFailed, protocol.StatusCancelled, protocol.StatusSkipped, protocol.StatusConfirmed,
		} {
			t.Run(string(status), func(t *testing.T) {
				_, anom := terminalize(t, status, "reason", transitID)
				if !anom {
					t.Errorf("stranded _TRANSIT bin NOT stamped anomalous on %s, want stamped", status)
				}
			})
		}
	})
}
