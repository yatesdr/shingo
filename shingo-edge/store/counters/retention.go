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
// CORE ON PURPOSE. Core keeps the same observations for 90 days in
// cell_part_events — the raw stream, field for field, projected from the
// same production.tick this table's rows generate — on a Proxmox VM with
// real disk. The Pi keeps only what someone might have to look at without
// Core: 14× the outbox's own 24-hour retention, and roughly 6.5× the worst
// realistic Core-outage window (the Kafka no-retry wedge, which is
// days-scale). Nothing reads a row older than the popover's open anomalies.
//
// The size argument is secondary and, at today's six counters, weak: the
// table is 8.35 MB after 93 days and 35.94 bytes/row. It is the cell
// expansion that makes this worth doing — 40 counters is ~242 MB/year on
// an SD card. Against the restored Springfield database this window keeps
// 80,846 of 232,392 rows.
const SnapshotRetention = 14 * 24 * time.Hour

// PurgeOldSnapshots deletes counter_snapshots older than olderThan,
// preserving unconfirmed jumps at ANY age. Returns the number deleted.
//
// The preserved rows are the operator's popover: an unconfirmed jump is
// the only counter_snapshot with a live UI affordance
// (ListUnconfirmedAnomalies → loadAnomalyData → the navbar bell), and
// aging one out would silently discard units nobody has accepted or
// rejected. There is no age cap on them — the backlog on the Springfield
// dump was two rows, both a day old, five of seven jumps ever recorded
// having been confirmed, so there is no stale tail to cap.
//
// COALESCE IS NOT DECORATION. anomaly is a nullable TEXT column and
// SQLite's `NOT (anomaly = 'jump' AND operator_confirmed = 0)` is
// three-valued: for a row with anomaly NULL and operator_confirmed = 0 the
// inner AND is (NULL AND true) = NULL, NOT NULL is NULL, and the row fails
// the WHERE — retained forever, invisibly. Such rows do not exist on the
// dump only because plc/manager.go inserts `confirmed := anomaly != "jump"`
// while the column's schema DEFAULT is 0; any future writer that takes the
// default would seed rows this purge could never remove. COALESCE collapses
// it back to two-valued logic. Demonstrated against the restored
// Springfield database: both forms delete 151,546 rows as the data stands,
// and after seeding a single anomaly-NULL, unconfirmed row the bare form
// still deletes 151,546 while the COALESCE form deletes 151,547.
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
	res, err := db.Exec(`DELETE FROM counter_snapshots
		WHERE recorded_at < ?
		  AND NOT (COALESCE(anomaly, '') = 'jump' AND operator_confirmed = 0)`,
		cutoff.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
