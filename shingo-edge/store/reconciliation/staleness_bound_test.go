package reconciliation

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
	"shingo/protocol"
	"shingo/protocol/testutil"
)

// openAnomalyDB creates a fresh SQLite DB with just enough of the orders table
// for ListAnomalies' stuck-order query. The real schema is built by migrations
// that need the whole edge; this is the narrow slice the query reads.
func openAnomalyDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "anomaly.db"))
	testutil.MustNoErr(t, err, "open sqlite")
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`
CREATE TABLE orders (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    uuid        TEXT NOT NULL,
    status      TEXT NOT NULL,
    queue_code  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL
);
CREATE TABLE outbox (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    status       TEXT NOT NULL DEFAULT 'pending',
    attempts     INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);`)
	testutil.MustNoErr(t, err, "create schema")
	return db
}

// TestListAnomalies_MaterialWaitGetsTheLongerBound is the Edge mirror of Core's
// bound, and it exists because this package had NO tests at all — the bound was
// re-homed onto queue_code with nothing to catch it going wrong.
//
// The Edge keys on the CODE because the cause never crosses the wire. That makes
// it coarser than Core's by construction, which is stated at the query and
// asserted here: `waiting_for_material` covers both the real shortages and the
// fail-closed family, so an outage on an Edge order gets the long bound where
// Core would raise it at thirty minutes. Coarse is the accepted cost; SILENT
// would not be, and the previous rung-keyed spelling was about to become silent
// in the other direction — complex orders rest in `sourcing`, and are now born
// there, so every ordinary complex material wait would have alarmed at 30
// minutes.
func TestListAnomalies_MaterialWaitGetsTheLongerBound(t *testing.T) {
	t.Parallel()
	db := openAnomalyDB(t)

	mk := func(uuid, status, code, age string) {
		_, err := db.Exec(
			`INSERT INTO orders (uuid, status, queue_code, updated_at) VALUES (?, ?, ?, datetime('now', ?))`,
			uuid, status, code, age)
		testutil.MustNoErr(t, err, "insert "+uuid)
	}

	// An hour old, waiting on material: under the two-hour bound either way.
	mk("m-young", string(protocol.StatusSourcing), string(protocol.QueueWaitingForMaterial), "-1 hours")
	// Three hours on material: over it.
	mk("m-old", string(protocol.StatusQueued), string(protocol.QueueWaitingForMaterial), "-3 hours")
	// An hour dispatched with no code: the 30-minute bound applies.
	mk("d-old", string(protocol.StatusDispatched), "", "-1 hours")
	// An hour in `sourcing` on a LANE wait — the case the rung-keyed spelling
	// got wrong. `sourcing` used to take the short bound unconditionally.
	mk("s-lane", string(protocol.StatusSourcing), string(protocol.QueueStorageRearranging), "-1 hours")

	anomalies, err := ListAnomalies(db)
	testutil.MustNoErr(t, err, "ListAnomalies")

	flagged := map[string]bool{}
	for _, a := range anomalies {
		if a.Issue == "active_order_stuck" {
			flagged[a.OrderUUID] = true
		}
	}

	if flagged["m-young"] {
		t.Error("an order waiting an hour on material was flagged — a board that fires on ordinary " +
			"material churn is a board people learn to ignore")
	}
	if !flagged["m-old"] {
		t.Error("an order waiting three hours on material raised nothing — nothing else on the Edge " +
			"reports a waiting order")
	}
	if !flagged["d-old"] {
		t.Error("a dispatched order stale for an hour was not flagged; the longer bound must apply " +
			"to material waits ONLY, not widen to every row")
	}
	if !flagged["s-lane"] {
		t.Error("a `sourcing` order waiting an hour on a LANE was not flagged. The bound reads the " +
			"code, not the rung: a corridor that has not cleared in half an hour is a throughput " +
			"problem somebody should see, whatever rung the order is standing on.")
	}
}
