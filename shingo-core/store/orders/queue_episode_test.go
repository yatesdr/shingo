//go:build docker

// Black-box (package orders_test) per the cycle note in orders_test.go.
package orders_test

import (
	"database/sql"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// episodeCodes returns every history row for an order as (status, code) pairs,
// oldest first — the shape these assertions are about.
func episodeCodes(t *testing.T, db *sql.DB, orderID int64) [][2]string {
	t.Helper()
	rows, err := orders.ListHistory(db, orderID)
	testutil.MustNoErr(t, err, "list history")
	out := make([][2]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, [2]string{string(r.Status), r.Code})
	}
	return out
}

// TestSetQueueDetail_StampsTheEpisodeTheOrderIsRestingIn is the mis-attribution
// case, and it is the worse of the two the stamp had.
//
// The write targeted the order's last `'queued'` row. An order parked in
// `sourcing` — which is most parks, since the requeue arms all target sourcing
// and sourcing→sourcing self-skips — therefore had its cause stamped onto an
// EARLIER, DIFFERENT episode: the queued row it left minutes ago. The queue-code
// time series is the instrument the starvation work measures by, and it was
// being written against the wrong wait in both directions.
//
// So: two waits, two episodes, two causes. Each lands on its own row, and the
// row that belongs to the first wait is not rewritten by the second.
func TestSetQueueDetail_StampsTheEpisodeTheOrderIsRestingIn(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	o := newPendingOrder("uuid-episode-attribution")
	o.Status = protocol.StatusQueued
	testutil.MustNoErr(t, orders.Create(db, o), "create")

	// Episode 1: waiting in the line, under a lane cause.
	testutil.MustNoErr(t, orders.SetQueueDetail(db, o.ID,
		"Storage is being rearranged", string(protocol.QueueStorageRearranging), "lane-held-traffic"),
		"stamp episode 1")

	// Episode 2: it went shopping, and the hunt is what is now failing.
	moved, err := orders.UpdateStatusFrom(db, o.ID, string(protocol.StatusQueued), string(protocol.StatusSourcing), "reserving source bins")
	testutil.MustNoErr(t, err, "queued→sourcing")
	if !moved {
		t.Fatal("queued→sourcing was refused")
	}
	testutil.MustNoErr(t, orders.SetQueueDetail(db, o.ID,
		"Waiting for material: PART-A", string(protocol.QueueWaitingForMaterial), "reserve-holding"),
		"stamp episode 2")

	got := episodeCodes(t, db, o.ID)
	want := [][2]string{
		{string(protocol.StatusQueued), string(protocol.QueueStorageRearranging)},
		{string(protocol.StatusSourcing), string(protocol.QueueWaitingForMaterial)},
	}
	if len(got) != len(want) {
		t.Fatalf("history = %v, want one row per episode %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("history[%d] = %v, want %v.\n"+
				"A cause must land on the row of the wait it describes — never on a previous episode, "+
				"and never nowhere. Full history: %v", i, got[i], want[i], got)
		}
	}
}

// TestSetQueueDetail_ARepeatedStampStaysOnOneEpisode pins the half of the old
// behaviour that was right and must survive the re-aim: successive writes within
// ONE wait overwrite that wait's row rather than appending. The last known
// reason is why the order was still waiting.
func TestSetQueueDetail_ARepeatedStampStaysOnOneEpisode(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	o := newPendingOrder("uuid-episode-repeat")
	o.Status = protocol.StatusSourcing
	testutil.MustNoErr(t, orders.Create(db, o), "create")

	testutil.MustNoErr(t, orders.SetQueueDetail(db, o.ID,
		"Storage is being rearranged", string(protocol.QueueStorageRearranging), "lane-held-traffic"), "first")
	testutil.MustNoErr(t, orders.SetQueueDetail(db, o.ID,
		"Waiting for material: PART-A", string(protocol.QueueWaitingForMaterial), "reserve-holding"), "second")

	got := episodeCodes(t, db, o.ID)
	want := [][2]string{{string(protocol.StatusSourcing), string(protocol.QueueWaitingForMaterial)}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("history = %v, want %v — one wait is one row, carrying the last reason it was still waiting for", got, want)
	}
}

// TestSetQueueDetail_ALegacyEpisodeWithNoRowGetsOne is the never-dropped half.
//
// After the birth row lands, an order always has a row for the episode it is
// resting in. An order created BEFORE it does not, and a plant upgrades with
// live orders on the board. Rather than let those causes go the way the old
// stamp sent every complex order's — silently nowhere — the write inserts the
// missing row and the cause is recorded.
//
// The trade is stated: the inserted row's created_at is the moment the cause was
// written, not the moment the episode began, so a duration measured from it
// under-reads for exactly those orders. A cause on a slightly late row beats no
// cause at all, and the population empties itself within one order lifetime.
func TestSetQueueDetail_ALegacyEpisodeWithNoRowGetsOne(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	o := newPendingOrder("uuid-episode-legacy")
	o.Status = protocol.StatusSourcing
	testutil.MustNoErr(t, orders.Create(db, o), "create")
	// The pre-birth-row world: an order resting in an episode with no row.
	_, err := db.Exec(`DELETE FROM order_history WHERE order_id=$1`, o.ID)
	testutil.MustNoErr(t, err, "erase the birth row")

	testutil.MustNoErr(t, orders.SetQueueDetail(db, o.ID,
		"Waiting for material: PART-A", string(protocol.QueueWaitingForMaterial), "reserve-holding"), "stamp")

	got := episodeCodes(t, db, o.ID)
	want := [][2]string{{string(protocol.StatusSourcing), string(protocol.QueueWaitingForMaterial)}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("history = %v, want %v — a cause with no episode row to land on gets one written for it, not dropped", got, want)
	}
}

// TestSetQueueDetail_LeavesATerminalRowAlone guards the one row the stamp must
// never touch. `code` on a terminal row is a protocol.TermCode; overwriting it
// with a QueueCode would replace the record of how an order ENDED with a record
// of what it was waiting for. The old spelling could not reach a terminal row
// because it named 'queued'; re-aiming at the current episode can, so the guard
// becomes explicit.
func TestSetQueueDetail_LeavesATerminalRowAlone(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	o := newPendingOrder("uuid-episode-terminal")
	o.Status = protocol.StatusQueued
	testutil.MustNoErr(t, orders.Create(db, o), "create")
	_, err := db.Exec(
		`INSERT INTO order_history (order_id, status, detail, code) VALUES ($1, 'failed', 'gave up', 'no_source_bin')`, o.ID)
	testutil.MustNoErr(t, err, "seed the terminal row")
	_, err = db.Exec(`UPDATE orders SET status='failed' WHERE id=$1`, o.ID)
	testutil.MustNoErr(t, err, "terminalize")

	testutil.MustNoErr(t, orders.SetQueueDetail(db, o.ID,
		"Waiting for material: PART-A", string(protocol.QueueWaitingForMaterial), "reserve-holding"), "stamp")

	got := episodeCodes(t, db, o.ID)
	want := [][2]string{
		{string(protocol.StatusQueued), ""},
		{"failed", "no_source_bin"},
	}
	if len(got) != len(want) {
		t.Fatalf("history = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("history[%d] = %v, want %v — a terminal row records how the order ENDED and a queue code must not overwrite it", i, got[i], want[i])
		}
	}

	// AND THE LIVE COLUMNS TOO, which is the half that had no guard. The history
	// stamp refused, and the row itself still went on to read "waiting for
	// material" on every board that shows queue_reason — a finished order wearing
	// a wait. Both halves of one write answer the same question now.
	var reason, code, cause sql.NullString
	testutil.MustNoErr(t, db.QueryRow(
		`SELECT queue_reason, queue_code, queue_cause FROM orders WHERE id=$1`, o.ID).
		Scan(&reason, &code, &cause), "read the live columns")
	if reason.String != "" || code.String != "" || cause.String != "" {
		t.Errorf("a terminal order carries queue_reason=%q code=%q cause=%q, want all empty. "+
			"The order has ENDED; a wait sentence on its row tells every board it is still waiting "+
			"for something.", reason.String, code.String, cause.String)
	}
}
