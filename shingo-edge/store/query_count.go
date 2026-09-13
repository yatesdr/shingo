package store

// query_count.go — the ONE instrument for tests that pin "this path issues a
// constant number of queries".
//
// The edge store is pinned to a single SQLite connection (Open sets
// MaxOpenConns(1)), so a code path that issues one query per style serialises
// every other reader behind it, and the number of queries — not the rows — is
// what an operator feels as a hang. A test that asserts on wall time cannot
// hold that line (it is noise on a loaded CI runner); a test that counts
// statements can. Nothing in this module counted statements before this file
// (verified 2026-09-02: no driver wrapper, no sql.OpenDB, no query hook in any
// _test.go), so this is where the counting lives, and a second way should not
// be added beside it.
//
// It counts at the database/sql driver seam, so it sees exactly what the
// application sent: every Exec/Query, whether direct or through a prepared
// statement, inside or outside a transaction. It does NOT count what the
// driver does on its own (the BEGIN/COMMIT a Tx issues, the PRAGMAs the DSN
// applies at connect) — those are per connection or per transaction, not per
// row, and are not the thing that scales.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync/atomic"
)

// QueryCounter counts the statements a DB opened by OpenCounting has sent to
// SQLite since the last Reset. Safe for concurrent use.
type QueryCounter struct {
	n atomic.Int64
}

// Count returns the statements issued since the last Reset (or open).
func (c *QueryCounter) Count() int64 { return c.n.Load() }

// Reset zeroes the count. Call it after fixture seeding and before the call
// under test, so the count is the call's and not the setup's.
func (c *QueryCounter) Reset() { c.n.Store(0) }

// OpenCounting is Open with every statement counted. Same DSN pragmas, same
// single-connection pin, same migrate + verifySchema — the only difference is
// that the connection is reached through a counting driver.Connector rather
// than the registered "sqlite" name. Keep the open sequence in lockstep with
// Open: a test that counts against a differently-configured store is
// measuring something other than production.
//
// Test fixtures only. Production opens must use Open.
//
// WHY IT IS EXPORTED, IN package store, IN A FILE WITH NO BUILD TAG. It cannot
// be an export_test.go symbol: four packages' tests call it (engine, messaging,
// service and store itself), and export_test.go reaches one package. It cannot
// move to a store/storetest subpackage either, because it has to open the store
// EXACTLY as Open does or it is measuring something else — and that means
// dsnFor, (*DB).migrate, (*DB).verifySchema and the DB.path field, all four of
// them unexported. Moving it would export four internals to hide one
// constructor, which is the worse trade. A build tag is the other option and it
// takes the budget pins out of the gate, which is the point of having them.
//
// So it is exported deliberately. Reviewed and left in place 2026-09-13; the
// close-out report's "unexported behind export_test.go" was never true.
func OpenCounting(path string) (*DB, *QueryCounter, error) {
	// The registered modernc driver, reached through database/sql so this file
	// does not depend on the driver package's constructor staying exported.
	// sql.Open does not connect; it only resolves the name.
	probe, err := sql.Open("sqlite", "")
	if err != nil {
		return nil, nil, fmt.Errorf("open counting db: resolve driver: %w", err)
	}
	drv := probe.Driver()
	_ = probe.Close()

	counter := &QueryCounter{}
	// dsnFor, not a copy of it: a counting store opened with different
	// pragmas would be measuring something other than production, which is
	// the one thing a measurement must not do.
	sqlDB := sql.OpenDB(&countingConnector{drv: drv, dsn: dsnFor(path), counter: counter})
	sqlDB.SetMaxOpenConns(1)

	db := &DB{DB: sqlDB, path: path}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, nil, fmt.Errorf("migrate: %w", err)
	}
	if err := db.verifySchema(); err != nil {
		sqlDB.Close()
		return nil, nil, err
	}
	counter.Reset()
	return db, counter, nil
}

// countingConnector opens connections through the wrapped driver and hands
// back conns that count.
type countingConnector struct {
	drv     driver.Driver
	dsn     string
	counter *QueryCounter
}

func (cc *countingConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := cc.drv.Open(cc.dsn)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, counter: cc.counter}, nil
}

func (cc *countingConnector) Driver() driver.Driver { return cc.drv }

// countingConn implements every optional driver interface database/sql probes
// for, delegating to the wrapped conn when it implements the same one. The
// wrapper has to declare them: database/sql decides by type assertion on the
// conn it is handed, and a wrapper that only embeds driver.Conn would push
// every Query through Prepare, which is a different code path from the one
// production runs.
type countingConn struct {
	driver.Conn
	counter *QueryCounter
}

var (
	_ driver.ConnBeginTx        = (*countingConn)(nil)
	_ driver.ConnPrepareContext = (*countingConn)(nil)
	_ driver.ExecerContext      = (*countingConn)(nil)
	_ driver.QueryerContext     = (*countingConn)(nil)
	_ driver.Pinger             = (*countingConn)(nil)
	_ driver.SessionResetter    = (*countingConn)(nil)
	_ driver.Validator          = (*countingConn)(nil)
)

func (c *countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.Conn.Begin() //nolint:staticcheck // fallback path for drivers without ConnBeginTx
}

func (c *countingConn) Prepare(query string) (driver.Stmt, error) {
	s, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &countingStmt{Stmt: s, counter: c.counter}, nil
}

func (c *countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	var (
		s   driver.Stmt
		err error
	)
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		s, err = p.PrepareContext(ctx, query)
	} else {
		s, err = c.Conn.Prepare(query)
	}
	if err != nil {
		return nil, err
	}
	return &countingStmt{Stmt: s, counter: c.counter}, nil
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip // database/sql falls back to Prepare, counted there
	}
	c.counter.n.Add(1)
	return e.ExecContext(ctx, query, args)
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	c.counter.n.Add(1)
	return q.QueryContext(ctx, query, args)
}

func (c *countingConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *countingConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *countingConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// countingStmt counts each execution of a prepared statement.
type countingStmt struct {
	driver.Stmt
	counter *QueryCounter
}

var (
	_ driver.StmtExecContext  = (*countingStmt)(nil)
	_ driver.StmtQueryContext = (*countingStmt)(nil)
)

func (s *countingStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.counter.n.Add(1)
	return s.Stmt.Exec(args) //nolint:staticcheck // fallback path for drivers without StmtExecContext
}

func (s *countingStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.counter.n.Add(1)
	return s.Stmt.Query(args) //nolint:staticcheck // fallback path for drivers without StmtQueryContext
}

func (s *countingStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	s.counter.n.Add(1)
	if e, ok := s.Stmt.(driver.StmtExecContext); ok {
		return e.ExecContext(ctx, args)
	}
	return s.Stmt.Exec(namedToValues(args)) //nolint:staticcheck // fallback path
}

func (s *countingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.counter.n.Add(1)
	if q, ok := s.Stmt.(driver.StmtQueryContext); ok {
		return q.QueryContext(ctx, args)
	}
	return s.Stmt.Query(namedToValues(args)) //nolint:staticcheck // fallback path
}

func namedToValues(args []driver.NamedValue) []driver.Value {
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	return vals
}
