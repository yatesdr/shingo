package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"shingocore/config"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// DB wraps *sql.DB with application-level query methods.
// The underlying *sql.DB is safe for concurrent use. It is set once at
// Open and never swapped: holders keep the *sql.DB itself (R27).
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
// switch to a focused sprint if the absorption tripwires fire. Those are
// written down in the implementation plan (docs/plans/implementation-plan.md at the
// GitHub root, OUTSIDE this repo — it was never committed in-tree).
type DB struct {
	*sql.DB
	// tells is where an order's station tell is queued (SetStationTells);
	// nil queues none. Atomic: test engines share one DB in parallel.
	tells atomic.Pointer[stationTells]
	// driftAtBoot is the drift report Open took after migrating (SchemaDrift):
	// written once, before Open returns, and read by the Core Health strip.
	driftAtBoot []string
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
// EXCLUSIVE: every migration runs at boot, and an ALTER of `orders` or
// `order_history` meeting an open transaction on that table waits, aborts,
// fails migrate(), and Core does not start. (Until the squash at v184 every
// boot took those locks: the pre-baseline column adds ALTERed `orders` on the
// pool and the replayed baseline built indexes, ahead of migrations such as
// 55, 59 and 61.)
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
// DDL runs on pool connections (the baseline's one Exec takes *sql.DB; the
// runner in protocol/migrate takes any connection db.Begin hands it), so a
// per-session SET would have to bound a connection nobody chooses. A session
// default in the startup packet covers every statement wherever it runs, which
// is the property being bought.
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

// ResetDatabase empties the database so the next Open() starts fresh: schema
// public is dropped with everything in it and recreated empty, owned by the
// user that ran the reset (Core's own) and with PUBLIC's USAGE, which is what
// PostgreSQL gives a new database's public schema.
//
// The schema, not the tables. It dropped the tables only, and that left behind
// the function and the free-standing sequence the schema also holds
// (bin_types_family_of_one, bin_placement_seq). The baseline
// (store/schema/baseline.sql) creates both with a plain CREATE, so a boot that
// births the database on it after such a reset fails on "already exists", and
// every boot after it. TestResetDatabase_ThenBootTwice pins the empty schema
// and the boots after.
//
// One statement, so one transaction: a reset that cannot drop the schema
// changes nothing. Only the schema's owner may drop it. On PostgreSQL 15 and
// later that is the database's owner (pg_database_owner), so a Core user that
// owns its database drops it; a Core user granted rights on a database another
// role owns is refused (insufficient_privilege, 42501), as every non-superuser
// was on 14 and older (Core needs 15 anyway: the baseline's
// idx_cms_txn_one_movement is NULLS NOT DISTINCT). On that refusal the reset
// falls back to dropping every object in the schema instead
// (resetPublicObjects), which a user may do to the objects it created, and the
// schema itself stays as it was.
func ResetDatabase(cfg *config.DatabaseConfig) error {
	sqlDB, err := openPgx(&cfg.Postgres)
	if err != nil {
		return fmt.Errorf("connect for reset: %w", err)
	}
	defer sqlDB.Close()
	_, err = sqlDB.Exec(`DROP SCHEMA public CASCADE;
		CREATE SCHEMA public AUTHORIZATION CURRENT_USER;
		GRANT USAGE ON SCHEMA public TO PUBLIC`)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		if ferr := resetPublicObjects(sqlDB); ferr != nil {
			return fmt.Errorf("drop schema public was refused (%v), and dropping its objects failed: %w", err, ferr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("drop and recreate schema public: %w", err)
	}
	return nil
}

// resetPublicObjects drops every table, view, sequence and function in schema
// public, in one statement (so one transaction): the reset where the schema
// itself cannot be dropped. Names are read into arrays before the first drop,
// so a CASCADE that already took an object leaves only an IF EXISTS no-op.
// Partition children go with their parents. TestResetDatabase_ObjectFallbackThenBootTwice
// pins that it leaves nothing the baseline would collide with.
func resetPublicObjects(db *sql.DB) error {
	_, err := db.Exec(`DO $$
DECLARE
	drops text[];
	d text;
BEGIN
	SELECT coalesce(array_agg(stmt ORDER BY ord), '{}') INTO drops FROM (
		SELECT 1 AS ord, format('DROP %s IF EXISTS %I.%I CASCADE',
		       CASE c.relkind WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' ELSE 'TABLE' END,
		       n.nspname, c.relname) AS stmt
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'v', 'm')
		   AND NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid)
		UNION ALL
		SELECT 2, format('DROP SEQUENCE IF EXISTS %I.%I CASCADE', n.nspname, c.relname)
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'S'
		UNION ALL
		SELECT 3, format('DROP %s IF EXISTS %s CASCADE',
		       CASE p.prokind WHEN 'p' THEN 'PROCEDURE' WHEN 'a' THEN 'AGGREGATE' ELSE 'FUNCTION' END,
		       p.oid::regprocedure)
		  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = 'public'
	) s;
	FOREACH d IN ARRAY drops LOOP
		EXECUTE d;
	END LOOP;
END $$`)
	if err != nil {
		return fmt.Errorf("drop the objects in schema public: %w", err)
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
	drift, err := db.SchemaDrift()
	if err != nil {
		// The report refuses nothing, and a report it could not take is said once
		// and is not a reason to stop.
		drift = []string{"schema drift: not checked: " + err.Error()}
	}
	for _, line := range drift {
		log.Printf("store: %s", line)
	}
	db.driftAtBoot = drift
	return db, nil
}

// Ping opens a pool for cfg, pings it with a bounded deadline, and closes it.
// It never migrates and changes nothing: the config page's Test connection, and
// its save of a changed Database section, which refuses a database that does
// not answer and otherwise only writes the file. Nothing swaps a live pool: the
// lane lock and the ETA cache hold the *sql.DB they were built with, so a new
// database applies after a restart (R27).
//
// The probe is bounded: a misconfigured host (typo, stale DNS, firewall)
// surfaces a fast error instead of wedging inside database/sql's pool wait,
// which connect_timeout in the DSN does not reach.
func Ping(cfg *config.DatabaseConfig) error {
	db, err := OpenWithoutMigrate(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), (connectTimeoutSeconds+2)*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping new db: %w", err)
	}
	return nil
}
