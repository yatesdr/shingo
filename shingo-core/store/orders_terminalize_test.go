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
		_, terr := db.TerminalizeOrder(order.ID, status, detail)
		testutil.MustNoErr(t, terr, "TerminalizeOrder "+string(status))

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

// TestTerminalizeOrder_ConcurrentTerminalizers pins the compare-and-swap on the
// terminal chokepoint.
//
// Two callers can each hold a snapshot showing a live order and both pass
// lifecycle's guard — an operator cancel racing a scanner fail is the everyday
// pair. Unguarded, the second write flipped an already-terminal order to a
// different terminal, added a second terminal row to its audit trail, and fired
// a second actionMap entry for one order.
//
// The subtle half: the LOSER must still release this order's holds and commit.
// Every release is keyed on the order id and idempotent, so running them twice
// is a no-op — but returning early would strand a claim on a bin forever, which
// is precisely the leak this chokepoint exists to prevent. Idempotent release
// is an invariant here, not luck.
func TestTerminalizeOrder_ConcurrentTerminalizers(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	var transitID int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT id FROM nodes WHERE name='_TRANSIT'`).Scan(&transitID), "lookup _TRANSIT")

	order := testdb.CreateOrder(t, db)
	bin := testdb.CreateBinAtNode(t, db, "PART-A", transitID, "BIN-CAS-RACE")
	testdb.ClaimBinForTest(t, db, bin.ID, order.ID)

	// First terminalizer wins.
	won, err := db.TerminalizeOrder(order.ID, protocol.StatusCancelled, "operator cancel")
	testutil.MustNoErr(t, err, "first TerminalizeOrder")
	if !won {
		t.Fatal("first terminalizer must win")
	}

	// Second terminalizer, from its own stale non-terminal snapshot, loses.
	won2, err := db.TerminalizeOrder(order.ID, protocol.StatusFailed, "scanner fail")
	testutil.MustNoErr(t, err, "second TerminalizeOrder")
	if won2 {
		t.Error("second terminalizer must NOT win — terminal states are absorbing")
	}

	// The winner's status stands; the loser did not overwrite it.
	var status, errDetail string
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT status, error_detail FROM orders WHERE id=$1`, order.ID).Scan(&status, &errDetail), "read order")
	if status != string(protocol.StatusCancelled) {
		t.Errorf("status = %q, want %q — the loser overwrote the winner", status, protocol.StatusCancelled)
	}
	if errDetail != "operator cancel" {
		t.Errorf("error_detail = %q, want the winner's reason", errDetail)
	}

	// Exactly one terminal history row — the loser must not double-audit.
	var terminalRows int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT count(*) FROM order_history WHERE order_id=$1 AND status IN ('cancelled','failed')`,
		order.ID).Scan(&terminalRows), "count history")
	if terminalRows != 1 {
		t.Errorf("terminal history rows = %d, want 1", terminalRows)
	}

	// And the holds are released — by whichever call got there, but released.
	var claimedBy sql.NullInt64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT claimed_by FROM bins WHERE id=$1`, bin.ID).Scan(&claimedBy), "read bin claim")
	if claimedBy.Valid {
		t.Errorf("bin still claimed by order %d — a refused terminalize stranded the hold", claimedBy.Int64)
	}
}
