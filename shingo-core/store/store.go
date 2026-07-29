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
//	lock_timeout (ms): caps how long a statement WAITS FOR A LOCK. See below.
//
// The connection-health loop re-probes every 30s, so a short bound is
// the right shape — failed configs surface as "disconnected" in seconds,
// not minutes.
//
// WHY lock_timeout EXISTS, AND WHY IT IS NOT JUST "ANOTHER TIMEOUT".
//
// Without it, lock_timeout is 0 — wait forever — so statement_timeout is what
// bounds a lock wait, and it bounds it at THIRTY SECONDS. MEASURED, against a
// real Postgres 16, before this change: a waiter for ACCESS EXCLUSIVE on
// `orders` aborted after 29.9997793s with SQLSTATE 57014. That is the
// deploy-night hazard on DEPLOY-WATCH-LIST-2026-07-27 W-2e, reproduced.
//
// It matters at startup, because startup is where this database takes ACCESS
// EXCLUSIVE. migrateAddBaselineColumns ALTERs `orders` five times on the pool,
// schema.Apply builds indexes, and migrations 55/59/61 each take ACCESS
// EXCLUSIVE on `orders` or `order_history`. Any one of them meeting an open
// transaction on those tables waits, aborts, fails migrate(), and Core does not
// start.
//
// TWO THINGS CHANGE, and the second is the bigger one:
//
//  1. The failure arrives in 3s instead of 30s, and systemd's
//     Restart=always/RestartSec=5s (shingo-core.service:17-18) turns that into
//     an actual retry loop rather than a 35-second one.
//
//  2. It stops Core's start attempt from blockading the plant's own database.
//     A pending ACCESS EXCLUSIVE request QUEUES EVERY LATER LOCK REQUEST BEHIND
//     IT — that is Postgres lock-queue semantics, not a shingo behaviour — so
//     for as long as the ALTER waits, every other reader of `orders` waits too.
//     At the plants Postgres is off-box and SHARED (dashboards, reports, psql),
//     and Core is stopped during the install, so those other readers are the
//     only clients there are. A restart loop with a 30s wait per attempt holds
//     that blockade for most of every cycle; at 3s it holds it for a fraction.
//
// AND IT NAMES THE CAUSE. 55P03 lock_not_available says "something holds the
// lock". 57014 says "a statement was slow" and could be anything. On the night,
// the difference between those two codes in the journal is the difference
// between running the pg_stat_activity query in W-2e and guessing.
//
// THREE SECONDS, and it is bounded on both sides. Below statement_timeout by
// 10x or it would be dead code. Above every transaction this codebase actually
// holds — the rehearsed migrations complete in 4-81ms once they hold the lock,
// and the only explicit row-lock wait in shingo-core is one FOR UPDATE in
// store/recovery (recovery.go), with no advisory locks anywhere. So the
// behaviour change is confined to the 3s-30s band, and a statement that waited
// in that band previously did not succeed either — it aborted at 30s. Nothing
// that used to commit stops committing.
//
// IT APPLIES TO EVERY SESSION, not only the migration ones, deliberately. The
// DDL runs on pool connections (schema.Apply takes *sql.DB, runOneMigration
// takes any connection db.Begin hands it), so a per-session SET would have to
// bound a connection nobody chooses. A session default in the startup packet
// covers every statement wherever it runs, which is the property being bought.
const (
	connectTimeoutSeconds   = 5
	statementTimeoutSeconds = 30
	lockTimeoutSeconds      = 3
)

func dsn(cfg *config.PostgresConfig) string {
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s connect_timeout=%d statement_timeout=%d lock_timeout=%d",
		cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password, cfg.SSLMode,
		connectTimeoutSeconds, statementTimeoutSeconds*1000, lockTimeoutSeconds*1000)
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
