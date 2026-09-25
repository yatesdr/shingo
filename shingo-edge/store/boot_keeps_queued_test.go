package store

import (
	"path/filepath"
	"testing"

	"shingo/protocol/testutil"
)

// TestBoot_AQueuedOrderStaysQueuedWithItsReason — red at the base, green since
// the Pi-diet phase deleted the boot fixup.
//
// The migration chain runs on every Edge start, and its data fixups included
// `UPDATE orders SET status='pending' WHERE status='queued'`, unguarded. So an
// Edge restarted while one of its orders was waiting at Core under a cause
// showed that order as `pending` until Core's next update happened to arrive:
// the word that says "Core has it and is waiting on something" replaced by the
// one that says "not yet acknowledged", and the reason beside it read against
// the wrong status. Flat bug, SYNTH §0 (the boot-fixup row).
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

	if status != "queued" {
		t.Fatalf("a queued order came back %q after a restart, want queued: a restart does not change what "+
			"Core is doing with the order, and the reason beside it (%q) has to read against its own status",
			status, reason)
	}
}
