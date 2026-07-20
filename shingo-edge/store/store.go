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
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on", path)
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
