package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"shingocore/config"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
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
//
// The architectural terminus is *store.DB as a connection-lifecycle
// wrapper with zero application methods. The current path is absorption;
// switch to a focused sprint if the absorption tripwires (see
// implementation-plan.md) fire.
type DB struct {
	*sql.DB
}

// Timeout bounds for the Postgres driver. Without these, pgx/libpq
// inherits libpq's "wait forever" defaults — a misconfigured host
// (typo, stale DNS, firewall) wedges every caller of Open / Ping /
// Query for the full kernel TCP retransmission timeout, and a slow
// query stalls a goroutine indefinitely.
//
//	connect_timeout: bounds the initial TCP connect attempt.
//	pool_max_conn_lifetime / statement_timeout (ms): caps each query.
//
// The connection-health loop re-probes every 30s, so a short bound is
// the right shape — failed configs surface as "disconnected" in seconds,
// not minutes.
const (
	connectTimeoutSeconds   = 5
	statementTimeoutSeconds = 30
)

func dsn(cfg *config.PostgresConfig) string {
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s connect_timeout=%d statement_timeout=%d",
		cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password, cfg.SSLMode,
		connectTimeoutSeconds, statementTimeoutSeconds*1000)
}

// pgxConnConfig parses the DSN and pins the session TimeZone to UTC.
//
// This is load-bearing for correctness, not cosmetics. Every timestamp
// column in the schema is TIMESTAMPTZ, and Postgres interprets any
// *zoneless* timestamp literal using the session's TimeZone — which, left
// unset, inherits the database server's OS timezone (the core VMs are
// generic Linux, not guaranteed UTC). A zoneless literal written or
// compared on a non-UTC session is therefore silently shifted by the
// offset: the class of bug behind bins.ConfirmManifest (R20-1) and
// messaging.PurgeOldOutbox. Pinning the session to UTC makes that class
// impossible regardless of which code path builds a literal. It is a
// per-connection session default, so psql, dashboards, and other clients
// are unaffected. (Application code should still bind time.Time rather
// than format zoneless strings; this is defense in depth, not a licence.)
func pgxConnConfig(cfg *config.PostgresConfig) (*pgx.ConnConfig, error) {
	connConfig, err := pgx.ParseConfig(dsn(cfg))
	if err != nil {
		return nil, err
	}
	if connConfig.RuntimeParams == nil {
		connConfig.RuntimeParams = map[string]string{}
	}
	connConfig.RuntimeParams["timezone"] = "UTC"
	return connConfig, nil
}

// openPgx opens a *sql.DB backed by pgx with the UTC session pin applied.
func openPgx(cfg *config.PostgresConfig) (*sql.DB, error) {
	connConfig, err := pgxConnConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	return sql.OpenDB(stdlib.GetConnector(*connConfig)), nil
}

// ResetDatabase removes all data so the next Open() starts fresh.
func ResetDatabase(cfg *config.DatabaseConfig) error {
	sqlDB, err := openPgx(&cfg.Postgres)
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

// OpenWithoutMigrate connects to the configured Postgres database and
// applies pool limits, but does NOT run migrations. Production callers
// should use Open; this is a test-only seam so testdb can clone a
// pre-migrated template database and skip per-test migration cost.
// Lives next to Open so the two paths are obviously paired.
func OpenWithoutMigrate(cfg *config.DatabaseConfig) (*DB, error) {
	sqlDB, err := openPgx(&cfg.Postgres)
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

	return &DB{DB: sqlDB}, nil
}

func Open(cfg *config.DatabaseConfig) (*DB, error) {
	db, err := OpenWithoutMigrate(cfg)
	if err != nil {
		return nil, err
	}
	if err := db.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// Reconnect swaps the underlying database connection in-place.
// The old connection is closed after the swap. All holders of *DB
// see the new connection immediately. Brief overlap during the swap
// is safe because *sql.DB handles in-flight queries on the old pool.
//
// Connectivity probe FIRST, migration second. Pre-fix this path called
// Open(cfg) which ran migrate() before any ping; a misconfigured host
// (typo, stale DNS, firewall) wedged the migrate's QueryRow calls
// inside database/sql's pool wait — connect_timeout in the DSN didn't
// reach those code paths. Now we PingContext with a bounded deadline
// against the new pool before touching migrate, so an unreachable host
// surfaces a fast error and the engine stays on its existing
// connection.
func (db *DB) Reconnect(cfg *config.DatabaseConfig) error {
	newDB, err := OpenWithoutMigrate(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), (connectTimeoutSeconds+2)*time.Second)
	defer cancel()
	if err := newDB.PingContext(ctx); err != nil {
		newDB.Close()
		return fmt.Errorf("ping new db: %w", err)
	}
	if err := newDB.migrate(); err != nil {
		newDB.Close()
		return fmt.Errorf("migrate new db: %w", err)
	}
	old := db.DB
	db.DB = newDB.DB
	old.Close()
	return nil
}
