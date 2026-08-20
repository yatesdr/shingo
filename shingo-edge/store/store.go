package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite database connection.
//
// *store.DB method-surface convention (Phase 6.4b, 2026-04-25):
// target is no new methods on this receiver. Existing delegates
// retire opportunistically as services adopt store/<aggregate>
// sub-package calls directly.
//   - New persistence logic: store/<aggregate>/ as a function on *sql.DB.
//   - New cross-aggregate orchestration: shingoedge/service/.
//
// The architectural terminus is *store.DB as a connection-lifecycle
// wrapper with zero application methods. The current path is absorption;
// switch to a focused sprint if the absorption tripwires fire. Those are
// written down in the implementation plan (docs/plans/implementation-plan.md at the
// GitHub root, OUTSIDE this repo — it was never committed in-tree).
type DB struct {
	*sql.DB
}

// Transaction runs fn inside a single SQLite transaction. Commits if
// fn returns nil; rolls back on any error or panic. Callers that need
// to compose several store-level mutations atomically wrap them here.
//
// SQLite holds a single writer at a time (the busy_timeout DSN param
// queues concurrent writers); the engine's max-open-conns=1 setting
// makes that explicit. So nested Transaction calls deadlock — don't.
//
// Note for the loader empty-in path: the reservation seam
// (engine.withLoaderBudget) owns NO transaction and must not be given one.
// Its atomicity comes from a per-loader mutex, not DB isolation, because the
// only operation that raises a loader's in-flight count is the create it guards
// (monotone-safe) and CreateRetrieveOrder is not tx-pure (it enqueues to Core
// and emits synchronously mid-write). A surrounding tx would only add a
// busy_timeout stall on this single connection and a rollback path that can't
// undo the Core enqueue. See FINAL-ADJUDICATION Q1 —
// shingo-library/archive/bin-loader-multiwindow-reviews-2026-06-12/FINAL-ADJUDICATION.md.
func (db *DB) Transaction(fn func(*sql.Tx) error) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Open opens (or creates) a SQLite database and runs migrations.
//
// DSN parameter syntax is driver-specific and the two common SQLite drivers do
// NOT share it. This is modernc.org/sqlite (registered as "sqlite"); the
// mattn/go-sqlite3 spelling `_journal_mode=WAL&_busy_timeout=5000&
// _foreign_keys=on` was used here from the start and is silently ignored —
// modernc's applyQueryParams reads only `_pragma`, `_time_format` and
// `_txlock`, and both SQLite and the driver drop unrecognised URI parameters
// without complaint.
//
// That WAS the state on the Springfield edge on 2026-07-24, before the DSN was
// fixed: `PRAGMA journal_mode` returned `delete` and no -wal/-shm files existed
// beside the 31 MB database. It is no longer the state anywhere the fix is
// deployed, and the two plants differ — so do not read the paragraph above as a
// description of a live box. Springfield runs 4527c4d6, which contains this
// fix, and is genuinely in WAL. Hopkinsville runs ef99421f, which does not, and
// is still on the rollback journal. Anything that reasons about the on-disk
// sidecar files (backups, db-migration.sh, an operator with sqlite3) has to
// establish which of the two it is looking at, per plant, from the deployed
// commit — not from this file.
//
// journal_mode(WAL) suits this workload — many small writes (outbox drain,
// inventory deltas, PLC counters) on SD-card storage, where a rollback
// journal's create/write/fsync/delete per commit is the expensive path. The
// hourly wal_checkpoint(TRUNCATE) in cmd/shingoedge/main.go was written for
// this and was a no-op until the fix; deploy/db-migration.sh likewise already
// expects WAL and a regenerable -shm.
//
// busy_timeout(5000) makes a lock conflict wait rather than fail instantly.
// Inside this process there is only one connection (below), but every OTHER
// reader — backups, db-migration.sh, an operator running sqlite3 — was getting
// an immediate "database is locked".
//
// foreign_keys is deliberately NOT set here, and the modernc spelling that
// would set it is `_pragma=foreign_keys(1)` — the mattn `_foreign_keys=on`
// above reads as if it did and does not. Measured on the Springfield dump:
// with the mattn spelling `PRAGMA foreign_keys` returns 0, with the _pragma
// spelling it returns 1. Whichever way this ends up being set, assert the
// pragma's VALUE, never the DSN text (see open_pragmas_test.go).
//
// SQLite defaults enforcement off, so the schema's 18 ON DELETE CASCADE and 16
// ON DELETE SET NULL clauses (plus 3 NO ACTION) have never been enforced on any
// edge. The foreign_key_check audit this comment used to ask for has now been
// run against the Springfield dump of 2026-07-27, and the answer is that
// enabling enforcement is a three-step change, not a one-line one:
//
//  1. 11 REFERENCES clauses across 8 tables still name the pre-rename tables
//     (styles_legacy, reporting_points_legacy, processes_legacy). 8 name a table
//     that no longer exists at all. With enforcement on, 11 of 37 FK-touching
//     writes are refused — some as `no such table: main.styles_legacy`, some as
//     plain `FOREIGN KEY constraint failed` because processes_legacy still
//     exists but is empty. Repairing the schema text has to come first.
//  2. 1,764 rows genuinely reference parents that are gone. Several of those
//     groups are production history, so what happens to them is an owner
//     decision, not a migration detail.
//  3. Even with both done, deleting a style becomes IMPOSSIBLE rather than
//     merely lossy. styles -> reporting_points is CASCADE, but
//     counter_snapshots -> reporting_points is NO ACTION, so the cascade hits a
//     restrict and the whole delete is refused. Measured: of 8 styles probed, 6
//     (those with a reporting point that has snapshots) were refused and 2 were
//     deleted cleanly. That edge needs resolving before enforcement is a
//     behaviour anyone wants.
//
// The reason to want it: with enforcement off, deleting a style silently
// strands its claims. Measured on the same dump — deleting style 24 succeeds
// and takes the orphan-claim count from 8 to 11. That is the defect, and it is
// why this is worth finishing, not why it is safe to switch on today.
//
// STATUS 2026-07-27 — steps 1 and 3 are DONE IN CODE; step 2 is done in a
// runbook that has not been run at a plant. That is the whole gate now, and it
// is worth being precise about which parts moved:
//
//   - Step 3 is closed. counter_snapshots.reporting_point_id is ON DELETE
//     CASCADE (schema/sqlite_ddl.go, and migrateCounterSnapshotCascade for
//     databases that already exist). Re-probed on the repaired dump: 0 of 57
//     style deletions refused, where 6 of 8 were before.
//   - Step 1's GENERATOR is closed — db.rebuildTable now holds
//     legacy_alter_table across every rename-rebuild, so no new dangling clause
//     can be created. The EXISTING 11 on the Springfield disk are not, and no
//     code change can close them: they are stored schema text on a plant's file
//     and need RUNBOOK-0.5-edge-fk-repair-2026-07-27.md run on the box.
//   - Step 2 is rehearsed, not applied. Against the repaired dump the full
//     ordered plan takes 1,764 -> 1 in ~11 ms, and the 1 is a single
//     hourly_counts row on style 32 that 90-day retention removes on its own.
//
// So the flip is one word — `&_pragma=foreign_keys(1)` on the DSN below — and
// it is NOT taken here, because the condition is a property of a PLANT'S FILE
// and not of this repository. Measured on the three states of the Springfield
// database, using a probe that rewrites every FK-participating column:
//
//	unrepaired                      19 of 37 FK-column writes blocked
//	after the §0.5 clause repair    12 of 37
//	after the ordered data plan       1 of 37   (the style-32 hourly_counts row)
//
// Enable it only against an edge that has been through §0.5 and measured. The
// assertion in open_pragmas_test.go is where that decision gets made in the
// open, and it carries its own reason so changing it cannot be a silent edit.
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path) // + "&_pragma=foreign_keys(1)" — see open_pragmas_test.go before adding
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)

	db := &DB{sqlDB}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// migrate() cannot report a failed ADD COLUMN (see schema_assert.go), and a
	// stale binary migrates cleanly to its own older schema. Verify the result
	// against what this build actually needs before handing the DB out.
	if err := db.verifySchema(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// OpenMigrated opens a database that is ALREADY at this build's schema and
// skips the migration chain. verifySchema still runs: it is the assert that
// every table and column this build needs is present, so a file that is not
// actually migrated fails loudly instead of behaving subtly wrong.
//
// For test fixtures copied from a pre-migrated template — the engine package's
// test pool — where the migration chain's ~120 statements per open were the
// package's dominant cost. Production opens must use Open.
func OpenMigrated(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)

	db := &DB{sqlDB}
	if err := db.verifySchema(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// CheckpointWAL runs PRAGMA wal_checkpoint(TRUNCATE) to flush the
// write-ahead log back into the main database file and reclaim
// disk space. Without periodic checkpoints the WAL can grow large
// under sustained writes on SD-card storage.
func (db *DB) CheckpointWAL() error {
	_, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// VacuumFreeFraction is the share of the file that must be free pages
// before VacuumIfFragmented will rebuild it.
//
// This is what makes the post-purge VACUUM a ONE-TIME event without any
// "have I done this yet" flag to get wrong across restarts. auto_vacuum is
// NONE — measured 0 on the restored Springfield database, and by
// construction: it appears nowhere in shingo-edge and Open's DSN sets only
// busy_timeout and journal_mode, so SQLite's default stands and a DELETE
// returns pages to the freelist without shrinking the file.
//
// The value is set from the measurement, not chosen. Running the real
// 14-day purge against the restored Springfield database deletes 151,547
// rows and leaves freelist_count 1,322 of page_count 4,923 — 26.9%, and
// higher on the live Pi, which carries free space the restore does not.
// Steady state is the other end of the same measurement: each 6-hour pass
// deletes what the last six hours inserted, tens of kilobytes against a
// file in the tens of megabytes, a fraction of a percent. Two orders of
// magnitude separate the two cases, so anything from about 5% to about 25%
// fires exactly once and never again; 20% takes the measured 26.9% with
// margin rather than sitting a point and a half under it.
//
// Self-limiting, and it also catches the genuinely-fragmented case that a
// fixed "once per process" rule would miss.
const VacuumFreeFraction = 0.20

// VacuumIfFragmented rebuilds the database when free pages exceed
// VacuumFreeFraction of the file, reclaiming the disk a DELETE only
// returned to SQLite's freelist. Reports whether it ran.
//
// VACUUM is not free and this is a Pi on an SD card. It copies the whole
// file, so it needs free disk equal to the database size and holds an
// exclusive lock throughout: 463–570 ms measured on a 19 MB file on
// workstation NVMe, and at a conservative 20 MB/s sustained SD write a
// 100 MB file is a floor of ~10 s of I/O. Both numbers are floors, not
// estimates — no Pi measurement exists. That cost is the reason for the
// threshold rather than an unconditional post-purge rebuild.
func (db *DB) VacuumIfFragmented(minFreeFraction float64) (bool, error) {
	var pageCount, freeCount int64
	if err := db.QueryRow("PRAGMA page_count").Scan(&pageCount); err != nil {
		return false, fmt.Errorf("page_count: %w", err)
	}
	if err := db.QueryRow("PRAGMA freelist_count").Scan(&freeCount); err != nil {
		return false, fmt.Errorf("freelist_count: %w", err)
	}
	if pageCount <= 0 || float64(freeCount) < minFreeFraction*float64(pageCount) {
		return false, nil
	}
	// VACUUM cannot run inside a transaction. Open pins MaxOpenConns(1), so
	// no other statement from this process can be mid-flight.
	if _, err := db.Exec("VACUUM"); err != nil {
		return false, fmt.Errorf("vacuum: %w", err)
	}
	// CHECKPOINT, OR THE REBUILD RECLAIMS NOTHING YET. In WAL mode VACUUM
	// writes the rebuilt pages into the write-ahead log; the main file keeps
	// its old size until a checkpoint moves them across. Skipping this would
	// leave the Pi momentarily holding BOTH the full-size database and a WAL
	// containing a whole copy of it — the opposite of the intent — until the
	// hourly checkpoint ticker happened to come round.
	if err := db.CheckpointWAL(); err != nil {
		return true, fmt.Errorf("checkpoint after vacuum: %w", err)
	}
	return true, nil
}
