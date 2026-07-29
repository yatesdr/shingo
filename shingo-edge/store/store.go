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
// switch to a focused sprint if the absorption tripwires (see
// implementation-plan.md) fire.
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
// (engine.reserveLoaderEmpties) owns NO transaction and must not be given one.
// Its atomicity comes from a per-loader mutex, not DB isolation, because the
// only operation that raises a loader's in-flight count is the create it guards
// (monotone-safe) and CreateRetrieveOrder is not tx-pure (it enqueues to Core
// and emits synchronously mid-write). A surrounding tx would only add a
// busy_timeout stall on this single connection and a rollback path that can't
// undo the Core enqueue. See FINAL-ADJUDICATION Q1.
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
// without complaint. Confirmed on the Springfield edge 2026-07-24: `PRAGMA
// journal_mode` returned `delete`, and no -wal/-shm files existed beside the
// 31 MB database. So WAL and the busy timeout have never actually been on.
//
// journal_mode(WAL) suits this workload — many small writes (outbox drain,
// inventory deltas, PLC counters) on SD-card storage, where a rollback
// journal's create/write/fsync/delete per commit is the expensive path. The
// hourly wal_checkpoint(TRUNCATE) in cmd/shingoedge/main.go was written for
// this and has been a no-op until now; deploy/db-migration.sh likewise already
// expects WAL and a regenerable -shm.
//
// busy_timeout(5000) makes a lock conflict wait rather than fail instantly.
// Inside this process there is only one connection (below), but every OTHER
// reader — backups, db-migration.sh, an operator running sqlite3 — was getting
// an immediate "database is locked".
//
// foreign_keys is deliberately NOT set here. SQLite defaults it off, so the
// schema's 17 ON DELETE CASCADE and 15 ON DELETE SET NULL clauses have never
// been enforced on any edge. Turning them all on at once against a live plant
// database would activate dormant cascades — a delete that removed one row
// would start removing children — and would likely surface pre-existing
// violations as new runtime errors. That needs a PRAGMA foreign_key_check
// audit against a copy of a real plant DB and its own change; it is not a
// safe rider on a performance fix.
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
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
