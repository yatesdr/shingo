package store

import (
	"database/sql"
	"fmt"
	"time"

	"shingocore/config"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// DB wraps *sql.DB with application-level query methods.
// The underlying *sql.DB is safe for concurrent use. Reconnect()
// swaps the pointer; brief overlap during the swap is tolerable
// since the old pool drains gracefully.
//
// *store.DB method-surface convention (Phase 6.4b, 2026-04-25):
// target is no new methods on this receiver. Existing delegates
// retire opportunistically as services adopt store/<aggregate>
// sub-package calls directly.
//   - New persistence logic: store/<aggregate>/ as a function on *sql.DB.
//   - New cross-aggregate orchestration: shingocore/service/.
// The architectural terminus is *store.DB as a connection-lifecycle
// wrapper with zero application methods. The current path is absorption;
// switch to a focused sprint if the absorption tripwires (see
// implementation-plan.md) fire.
type DB struct {
	*sql.DB
}

func dsn(cfg *config.PostgresConfig) string {
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password, cfg.SSLMode)
}

// ResetDatabase removes all data so the next Open() starts fresh.
func ResetDatabase(cfg *config.DatabaseConfig) error {
	sqlDB, err := sql.Open("pgx", dsn(&cfg.Postgres))
	if err != nil {
		return fmt.Errorf("connect for reset: %w", err)
	}
	defer sqlDB.Close()
	_, err = sqlDB.Exec(`DO $$ DECLARE r RECORD;
		BEGIN
			FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' LOOP
				EXECUTE 'DROP TABLE IF EXISTS public.' || quote_ident(r.tablename) || ' CASCADE';
			END LOOP;
		END $$`)
	if err != nil {
		return fmt.Errorf("drop tables: %w", err)
	}
	return nil
}

func Open(cfg *config.DatabaseConfig) (*DB, error) {
	sqlDB, err := sql.Open("pgx", dsn(&cfg.Postgres))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Connection pool limits — defaults if not set in config
	maxOpen := cfg.Postgres.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 25
	}
	maxIdle := cfg.Postgres.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = 10
	}
	maxLife := cfg.Postgres.ConnMaxLifetime
	if maxLife <= 0 {
		maxLife = 5 * time.Minute
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(maxLife)

	db := &DB{DB: sqlDB}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// Reconnect swaps the underlying database connection in-place.
// The old connection is closed after the swap. All holders of *DB
// see the new connection immediately. Brief overlap during the swap
// is safe because *sql.DB handles in-flight queries on the old pool.
func (db *DB) Reconnect(cfg *config.DatabaseConfig) error {
	newDB, err := Open(cfg)
	if err != nil {
		return err
	}
	if err := newDB.Ping(); err != nil {
		newDB.Close()
		return fmt.Errorf("ping new db: %w", err)
	}
	old := db.DB
	db.DB = newDB.DB
	old.Close()
	return nil
}
