package main

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"shingocore/store"
)

// scalar_test.go — the pins on soakstat's query-error contract.
//
// scalar used to return 0 on a failed query, then -1. Both were read as clean:
// every check asks `n > 0`. That swallow is why checkNegativeTotalUOP's
// `SUM(uop_count)` (a column that does not exist — the schema's is
// uop_remaining) read as a healthy zero for its whole life: the probe never
// ran, and no run of the soak ever executed the inventory invariant it claimed
// to check. The non-scalar query sites had the same swallow in another shape:
// an error became "no rows", which is also "nothing wrong".
//
// So scalar returns the error, and every instrument — each invariant check and
// each measure — reports FAILED TO RUN when its query fails. These tests drive
// every instrument against a database whose every query fails; the docker twin
// (every_query_runs_docker_test.go) runs every query against the real schema.
//
// VERIFIED RED BY: reverting one check's error arm to the old swallow
// (checkDoubleOccupancy returning out on a scalar error) — its row fails; and
// reverting one Scan arm to `continue` / deleting one rows.Err() check — the
// scanfail / iterfail rows fail (evidence-lead/fixB-scan-*.txt).

// stubMode is how the stub database misbehaves.
type stubMode int

const (
	stubOK       stubMode = iota // every query answers one row: 42
	stubFail                     // every query errors before returning rows
	stubScanFail                 // every query returns one row that no Scan here can decode
	stubIterFail                 // every query's first Next fails: rows.Err() carries the error
)

type stubDriver struct{ mode stubMode }

var (
	errQuery = errors.New("no such table: bins (test stub)")
	errIter  = errors.New("connection reset mid-result (test stub)")
)

func (d *stubDriver) Open(name string) (driver.Conn, error) { return stubConn{mode: d.mode}, nil }

type stubConn struct{ mode stubMode }

func (c stubConn) Prepare(query string) (driver.Stmt, error) {
	if c.mode == stubFail {
		return nil, errQuery
	}
	return stubStmt(c), nil
}
func (stubConn) Close() error                { return nil }
func (c stubConn) Begin() (driver.Tx, error) { return nil, errQuery }

type stubStmt struct{ mode stubMode }

func (stubStmt) Close() error  { return nil }
func (stubStmt) NumInput() int { return -1 }
func (stubStmt) Exec(args []driver.Value) (driver.Result, error) {
	return nil, errQuery
}
func (s stubStmt) Query(args []driver.Value) (driver.Rows, error) {
	return &stubRows{mode: s.mode}, nil
}

type stubRows struct {
	mode     stubMode
	consumed bool
}

func (r *stubRows) Columns() []string { return []string{"n"} }
func (r *stubRows) Close() error      { return nil }
func (r *stubRows) Next(dest []driver.Value) error {
	if r.mode == stubIterFail {
		return errIter
	}
	if r.consumed {
		return io.EOF
	}
	r.consumed = true
	if r.mode == stubScanFail {
		// ONE column: every multi-destination Scan in soakstat fails on the
		// column count, and a one-int Scan fails on the conversion.
		dest[0] = "not-a-number"
		return nil
	}
	dest[0] = int64(42)
	return nil
}

func init() {
	sql.Register("scalarstub-fail", &stubDriver{mode: stubFail})
	sql.Register("scalarstub-ok", &stubDriver{mode: stubOK})
	sql.Register("scalarstub-scanfail", &stubDriver{mode: stubScanFail})
	sql.Register("scalarstub-iterfail", &stubDriver{mode: stubIterFail})
}

// brokenDrivers are the three ways a query can fail to produce a reading.
var brokenDrivers = []string{"scalarstub-fail", "scalarstub-scanfail", "scalarstub-iterfail"}

func stubDB(t *testing.T, driverName string) *store.DB {
	t.Helper()
	sqlDB, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open stub: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return &store.DB{DB: sqlDB}
}

func TestScalarSurfacesQueryErrors(t *testing.T) {
	t.Parallel()
	db := stubDB(t, "scalarstub-fail")

	n, err := scalar(db, `SELECT COUNT(*) FROM bins`)
	if err == nil {
		t.Errorf("scalar on a failed query returned (%d, nil) — a value with no error is the "+
			"swallow that let a probe over a nonexistent column read as a healthy zero for its "+
			"whole life (checkNegativeTotalUOP, uop_count)", n)
	}
}

// TestScalarReturnsTheValueOnSuccess pins the success arm so the error cannot
// quietly become the only answer: a scalar that always failed would make every
// soak report unreadable probes and no numbers.
func TestScalarReturnsTheValueOnSuccess(t *testing.T) {
	t.Parallel()
	db := stubDB(t, "scalarstub-ok")

	if n, err := scalar(db, `SELECT 42`); err != nil || n != 42 {
		t.Errorf("scalar on a working query returned (%d, %v), want (42, nil)", n, err)
	}
}

// TestEveryCheckReportsFailedToRunOnABrokenDB: every invariant check, run
// against a database that fails in each of the three ways — the query errors,
// a row cannot be decoded (Scan), or the result set breaks mid-iteration
// (rows.Err) — must say FAILED TO RUN and say nothing else. An empty result
// here is the old swallow: the check never read its rows and the soak would
// have called it clean.
func TestEveryCheckReportsFailedToRunOnABrokenDB(t *testing.T) {
	t.Parallel()
	for _, drv := range brokenDrivers {
		db := stubDB(t, drv)
		for _, c := range invariantChecks {
			out := c.run(db)
			if len(out) == 0 {
				t.Errorf("[%s] check %q returned nothing on a database that cannot answer it — "+
					"its failure reads as a clean bill", drv, c.name)
			}
			for _, l := range out {
				if !strings.HasPrefix(l, failedToRunPrefix) {
					t.Errorf("[%s] check %q reported %q on a broken database — want only %q lines",
						drv, c.name, l, failedToRunPrefix)
				}
			}
		}
	}
}

// TestEveryMeasureReportsFailedToRunOnABrokenDB: the measures half, for the
// same three failure shapes. A broken database must leave every section
// marked FAILED-TO-RUN, no violation invented, the summary line carrying the
// count — never a row of zeros — and a non-zero exit.
func TestEveryMeasureReportsFailedToRunOnABrokenDB(t *testing.T) {
	t.Parallel()
	for _, drv := range brokenDrivers {
		r := collect(stubDB(t, drv), "")
		if len(r.violations) != 0 {
			t.Errorf("[%s] a broken database produced violations %v — failures must be failed-to-run, not findings", drv, r.violations)
		}
		for _, m := range []string{measureDigs, measureReuse, measureLanes, measureGated,
			measureDwell, measureDepth, measureDissolves, measureCauses} {
			if !r.unrunMeasures[m] {
				t.Errorf("[%s] measure %s did not report FAILED TO RUN on a broken database", drv, m)
			}
		}
		c := r.orders
		for name, rd := range map[string]reading{"total": c.total, "completed": c.completed,
			"failed": c.failed, "cancelled": c.cancelled, "in-flight": c.inFlight, "queued": c.queued} {
			if !rd.failed {
				t.Errorf("[%s] order count %s read %d on a broken database — want FAILED-TO-RUN", drv, name, rd.n)
			}
		}
		if r.dissolves.String() != "FAILED-TO-RUN" {
			t.Errorf("[%s] dissolves renders %q on a broken database — want FAILED-TO-RUN", drv, r.dissolves)
		}
		if len(r.failedToRun) == 0 || !strings.Contains(r.summary(), "failed-to-run ") ||
			strings.Contains(r.summary(), "failed-to-run 0") {
			t.Errorf("[%s] summary %q does not carry the failed-to-run count (%d)", drv, r.summary(), len(r.failedToRun))
		}
		if r.exitCode() == 0 {
			t.Errorf("[%s] a run whose instruments did not run exits 0 — it must exit non-zero", drv)
		}
	}
}

// TestACleanRunExitsZero keeps exitCode from failing everything: with no
// violation and nothing unrun, the soak exits 0.
func TestACleanRunExitsZero(t *testing.T) {
	t.Parallel()
	if got := (&report{}).exitCode(); got != 0 {
		t.Errorf("an empty clean report exits %d, want 0", got)
	}
	if got := (&report{violations: []string{"x"}}).exitCode(); got == 0 {
		t.Errorf("a report with a violation exits 0")
	}
}
