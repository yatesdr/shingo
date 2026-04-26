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
// The architectural terminus is *store.DB as a connection-lifecycle
// wrapper with zero application methods. The current path is absorption;
// switch to a focused sprint if the absorption tripwires (see
// implementation-plan.md) fire.
type DB struct {
	*sql.DB
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
	return db, nil
}
