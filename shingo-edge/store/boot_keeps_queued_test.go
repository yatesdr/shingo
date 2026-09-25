package store

import (
	"path/filepath"
	"testing"

	"shingo/protocol/testutil"
)

// TestBoot_AQueuedOrderStaysQueuedWithItsReason — VERIFY-RED at the base.
//
// The migration chain runs on every Edge start, and its data fixups include
// `UPDATE orders SET status='pending' WHERE status='queued'`, unguarded. So an
// Edge restarted while one of its orders is waiting at Core under a cause shows
// that order as `pending` until Core's next update happens to arrive: the word
// that says "Core has it and is waiting on something" is replaced by the one
// that says "not yet acknowledged", and the reason beside it reads against the
// wrong status.
//
// The Pi-diet phase deletes the fixup (SYNTH §0, the boot-fixup row; §4, "−1
// statement per boot; the cause survives a restart"). This pin is written before
// that, and is committed passing-while-red: it logs the reset while the fixup
// stands and fails the moment it is gone, so whoever deletes the fixup turns it
// into the plain assertion in the same commit.
//
// The first open is the one that creates the schema; the order is written
// between the two opens, as a running Edge would have left it.
func TestBoot_AQueuedOrderStaysQueuedWithItsReason(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "boot.db")

	db, err := Open(dbPath)
	testutil.MustNoErr(t, err, "first open")
	_, err = db.Exec(`INSERT INTO orders (uuid, order_type, status) VALUES ('Q-1', 'retrieve', 'queued')`)
	testutil.MustNoErr(t, err, "seed a queued order")
	testutil.MustNoErr(t, db.SetOrderQueueReason("Q-1", "Waiting for material: P1", "waiting_for_material"),
		"set its queue reason")
	testutil.MustNoErr(t, db.Close(), "close")

	db, err = Open(dbPath)
	testutil.MustNoErr(t, err, "reopen, as a restart does")
	defer db.Close()

	var status, reason, code string
	testutil.MustNoErr(t, db.QueryRow(`SELECT status, queue_reason, queue_code FROM orders WHERE uuid='Q-1'`).
		Scan(&status, &reason, &code), "read back")
	if reason != "Waiting for material: P1" || code != "waiting_for_material" {
		t.Fatalf("the restart cleared the reason (%q, %q); the pin assumes it survives and only the status "+
			"moves — rebuild it", reason, code)
	}

	// VERIFY-RED: predicted `pending` while the fixup stands.
	if status == "queued" {
		t.Fatalf("VERIFY-RED boot fixup is GREEN: the order came back `queued`. If the fixup was deleted, " +
			"replace this block with an assertion that status is `queued`; if not, the fixture no longer " +
			"reaches it and has to be rebuilt.")
	}
	if status != "pending" {
		t.Fatalf("the restart moved a queued order to %q; the predicted red is `pending`", status)
	}
	t.Logf("VERIFY-RED boot fixup, as predicted: a queued order came back %q beside the reason %q", status, reason)
}
