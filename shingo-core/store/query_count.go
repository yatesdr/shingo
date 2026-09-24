package store

// query_count.go — the ONE instrument for Core tests that pin "this path issues
// a constant number of statements".
//
// WHY CORE NEEDS ONE AT ALL, when the pressure argument that produced the Edge's
// counter does not apply here. The Edge store is one SQLite connection on a Pi,
// so a statement is a thing the next reader waits behind. Core has a 25-slot
// Postgres pool and no such queue. What Core has instead is a pair of recompute
// paths with very different budgets — a 300 ms debounce fired by bin movement,
// and a two-minute full pass — reading through ONE shared BuildInputs. A query
// added for the full pass lands on the debounce too, at a cadence set by plant
// traffic rather than by a timer, and the cost is invisible in every test that
// asserts on behaviour. B6 did exactly that (ActiveStyles inside BuildInputs)
// and its report recorded "zero on the 300 ms debounce". A count is what makes
// that claim checkable.
//
// It counts at pgx's own QueryTracer seam rather than by wrapping a
// database/sql driver, which is what the Edge file does. The seam is different
// because the driver is: Core reaches Postgres through stdlib.GetConnector on a
// *pgx.ConnConfig, so the tracer is a field on the config the store already
// builds, and using it means this file does not have to restate the connection
// setup. TraceQueryStart fires once for every statement pgx sends as a query —
// Query, Exec, and each execution of a prepared statement alike — and that
// includes a transaction's BEGIN and its COMMIT or ROLLBACK, which pgx issues
// as statements of their own: a transaction with one INSERT counts 3
// (measured by messaging.TestLinesideReport_StatementsPerEnvelope). It does
// not fire for the protocol traffic pgx does without a statement (Parse and
// Describe on a statement-cache miss). A budget pin that opens a transaction
// therefore counts its two brackets; every budget in this repo does.
//
// TEST FIXTURES ONLY. Production opens must use Open.
//
// WHY IT IS EXPORTED, IN package store, IN A FILE WITH NO BUILD TAG — the same
// three reasons the Edge's OpenCounting carries, and they hold here for the
// same shapes. It cannot be an export_test.go symbol: the budget pins live in
// engine and the read-layer ones in store/sourceability, and export_test.go
// reaches one package. It cannot move to internal/testdb either, because it has
// to open the database EXACTLY as production does or it is measuring something
// else — and that means pgxConnConfig and the pool defaults, both unexported.
// A build tag is the other option and it takes the budget pins out of the gate,
// which is the point of having them.

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"shingocore/config"
)

// QueryCounter counts the statements a DB opened by OpenCounting has sent to
// Postgres since the last Reset. Safe for concurrent use — the recompute paths
// this instruments run off timers and event goroutines.
type QueryCounter struct {
	n atomic.Int64
}

// Count returns the statements issued since the last Reset (or open).
func (c *QueryCounter) Count() int64 { return c.n.Load() }

// Reset zeroes the count. Call it after fixture seeding and before the call
// under test, so the count is the call's and not the setup's.
func (c *QueryCounter) Reset() { c.n.Store(0) }

var _ pgx.QueryTracer = (*QueryCounter)(nil)

// TraceQueryStart is the count. One call per statement the application hands
// pgx; the context is returned unchanged because nothing here needs per-query
// state.
func (c *QueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

// TraceQueryEnd is required by the interface and deliberately does nothing:
// counting at the end would miss a statement that errored, and an errored
// statement is one the database did work for.
func (c *QueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// OpenCounting is OpenWithoutMigrate with every statement counted. Same DSN,
// same UTC session pin, same pool limits — the only difference is the tracer.
// Keep the open sequence in lockstep with OpenWithoutMigrate: a counting store
// opened with a different configuration is measuring something other than
// production, which is the one thing a measurement must not do.
//
// It does NOT migrate, for the same reason OpenWithoutMigrate does not: the
// caller is a test holding a database cloned from an already-migrated template,
// and running the migrations again would put several hundred statements in
// front of the first Reset.
func OpenCounting(cfg *config.DatabaseConfig) (*DB, *QueryCounter, error) {
	connConfig, err := pgxConnConfig(&cfg.Postgres)
	if err != nil {
		return nil, nil, fmt.Errorf("open counting db: parse dsn: %w", err)
	}
	counter := &QueryCounter{}
	connConfig.Tracer = counter

	sqlDB := sql.OpenDB(stdlib.GetConnector(*connConfig))

	// The same three defaults OpenWithoutMigrate applies. Restated rather than
	// shared because they are four lines and the alternative is a helper whose
	// only job is to be called twice; if they ever grow, they move together or
	// this file stops measuring production.
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

	counter.Reset()
	return &DB{DB: sqlDB}, counter, nil
}
