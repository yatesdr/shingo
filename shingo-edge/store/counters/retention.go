package counters

// Retention for counter_snapshots — the Edge's one unbounded table with a
// real growth driver. Split from counters.go because it is a lifecycle
// concern with its own schedule (the retention ticker in
// cmd/shingoedge/main.go), not part of the CRUD surface the poll loop and
// the HMI share.

import (
	"database/sql"
	"time"
)

// SnapshotRetention is how long the Edge keeps counter_snapshots.
//
// FOURTEEN DAYS IS A WORKING WINDOW, NOT AN ARCHIVE, AND IT DIFFERS FROM
// CORE ON PURPOSE. Core keeps 90 days in cell_part_events on a Proxmox VM
// with real disk — but NOT the same observations field for field: Core gets
// only the shippable rows (delta > 0, a style), without reporting_point_id or
// operator_confirmed. The Pi keeps what someone might
// have to look at without Core, roughly 6.5× the worst realistic Core-outage
// window (the Kafka no-retry wedge, which is days-scale).
//
// THIS TABLE IS ALSO THE PRODUCTION TICK FEED'S QUEUE. The shipper
// (messaging.TickShipper) sends from it past a cursor, so this window bounds
// how long the feed can be down and still catch up: a row purged before it
// shipped is a tick Core never gets. That shipper is the only reader.
//
// The size argument is secondary and, at today's six counters, weak: the
// table is 8.35 MB after 93 days and 35.94 bytes/row. It is the cell
// expansion that makes this worth doing — 40 counters is ~242 MB/year on
// an SD card. Against the restored Springfield database this window keeps
// 80,846 of 232,392 rows.
const SnapshotRetention = 14 * 24 * time.Hour

// PurgeOldSnapshots deletes counter_snapshots older than olderThan. Returns
// the number deleted.
//
// EVERY ROW AGES OUT, JUMPS INCLUDED. This used to keep unconfirmed jumps at
// any age, because they were the operator's popover: units nobody had
// accepted or rejected. A jump is now counted at the poll like any delta
// (close-out 2b), so there is nothing left to accept, and the jumps a
// previous build left unconfirmed are a record like every other row.
//
// NO INDEX ON recorded_at, DELIBERATELY — and on the size argument only.
// Re-measured against the restored Springfield database rather than taken
// on trust:
//
//	index on the full table   6,504,448 B (6.20 MB), 0.4–0.7 s to build
//	table kept by this window 80,846 rows ≈ 2.8 MB
//	steady-state pass         ~13 ms without, ~0.2 ms with
//
// So the index is 2.2x the size of the entire table it would serve, to
// save about 13 ms on a background task that runs four times a day. That
// is decisive and it is deterministic.
//
// The other half of the case against it — that the index makes the one big
// backfill delete SLOWER — DID NOT REPRODUCE and should not be repeated.
// Six trials per arm on this host span 580–2,078 ms unindexed and
// 1,081–3,207 ms indexed, and an earlier pair of runs put them the other
// way round (730–816 ms against 542–634 ms). The delete timing here is
// host noise, not a signal, in either direction.
func PurgeOldSnapshots(db *sql.DB, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	// recorded_at is TEXT defaulted from datetime('now') — UTC,
	// second-granularity, 'YYYY-MM-DD HH:MM:SS'. That format sorts
	// lexicographically, so a string comparison is a chronological one, but
	// only if the bound value is rendered in the same shape and zone.
	res, err := db.Exec(`DELETE FROM counter_snapshots WHERE recorded_at < ?`,
		cutoff.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
