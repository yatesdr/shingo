//go:build docker

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"shingocore/internal/testdb"
)

// The DSN's timeouts are ASSERTED AS SESSION VALUES, not as substrings of the
// connection string.
//
// This exists because the same class of defect is already recorded against the
// other half of this repository: shingo-edge's SQLite DSN carried `_journal_mode`
// and `_busy_timeout` pragmas that the modernc driver does not parse, so for
// months the Edge ran with no WAL, no busy timeout and foreign keys never
// enforced — and every test that looked at the DSN STRING passed. A timeout that
// is set but not honoured is worse than one that is absent, because it reads as
// coverage.
//
// pgx is not modernc and the mechanism is different (pgconn.ParseConfig puts any
// setting outside its notRuntimeParams list into RuntimeParams, which ride the
// startup packet as session defaults), but "different driver, therefore fine" is
// an argument, not a measurement. These two tests are the measurement. They go
// through testdb.Open, which calls store.OpenWithoutMigrate → openPgx →
// pgxConnConfig → dsn(): the production path, not a hand-built connection.

// TestSessionTimeouts_AreHonouredByPostgres reads the two timeouts back out of
// a real server. current_setting is what the backend will actually enforce.
func TestSessionTimeouts_AreHonouredByPostgres(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	for _, tc := range []struct{ setting, want string }{
		{"lock_timeout", "3s"},
		{"statement_timeout", "30s"},
	} {
		var got string
		if err := db.QueryRow(`SELECT current_setting($1)`, tc.setting).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", tc.setting, err)
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q — the DSN carries it but the session does not",
				tc.setting, got, tc.want)
		}
	}
}

// TestSessionTimeouts_LockWaitAbortsAsLockTimeout is the behavioural half, and
// it is the assertion that matters.
//
// The deploy hazard (DEPLOY-WATCH-LIST W-2e) is a migration's ACCESS EXCLUSIVE
// request meeting an open transaction on `orders`. LOCK TABLE ... IN ACCESS
// EXCLUSIVE MODE takes the identical lock an ALTER TABLE takes, without needing
// a schema change to roll back, so it reproduces the hazard exactly.
//
// THE SQLSTATE IS THE WHOLE POINT. Both timeouts abort the waiter; they abort it
// with different codes at different times, and only one of them says why:
//
//	55P03 lock_not_available                     — something holds the lock
//	57014 canceling statement due to ... timeout — could be anything slow
//
// So a test that asserted only "it failed" would pass with lock_timeout absent.
// Asserting 55P03 is what proves lock_timeout fired and not statement_timeout,
// and asserting the elapsed time is under a fraction of statement_timeout is
// what proves it fired at ITS bound rather than the other one's.
func TestSessionTimeouts_LockWaitAbortsAsLockTimeout(t *testing.T) {
	// No t.Parallel: this one asserts on wall-clock elapsed time and the
	// blocker holds a table lock for the duration.
	db := testdb.Open(t)
	ctx := context.Background()

	// A dedicated connection so the blocking transaction cannot be handed the
	// same backend as the waiter below.
	blocker, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("dedicated conn for blocker: %v", err)
	}
	defer blocker.Close()

	held, err := blocker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin blocker tx: %v", err)
	}
	defer held.Rollback()
	if _, err := held.ExecContext(ctx, `LOCK TABLE orders IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("blocker LOCK TABLE: %v", err)
	}

	waiter, err := db.Begin()
	if err != nil {
		t.Fatalf("begin waiter tx: %v", err)
	}
	defer waiter.Rollback()

	start := time.Now()
	_, err = waiter.Exec(`LOCK TABLE orders IN ACCESS EXCLUSIVE MODE`)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("waiter acquired ACCESS EXCLUSIVE while the blocker held it — "+
			"impossible unless the blocker's lock was released (elapsed %s)", elapsed)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("waiter error is not a *pgconn.PgError: %T %v", err, err)
	}
	if pgErr.Code != "55P03" {
		t.Fatalf("waiter aborted with SQLSTATE %s (%q) after %s, want 55P03 lock_not_available.\n"+
			"57014 means statement_timeout fired instead, i.e. lock_timeout is not in force.",
			pgErr.Code, pgErr.Message, elapsed)
	}

	// Bounds, not an equality: the assertion is "it aborted at lock_timeout's
	// bound, not statement_timeout's". 1s floor catches a lock_timeout so short
	// it would abort ordinary contention; 15s ceiling is half of
	// statement_timeout, so nothing between the two can be confused.
	if elapsed < time.Second {
		t.Errorf("aborted after %s — under 1s, so lock_timeout is far shorter than intended", elapsed)
	}
	if elapsed > 15*time.Second {
		t.Errorf("aborted after %s — that is statement_timeout territory, not lock_timeout's", elapsed)
	}

	// AND THE BLOCKADE IS WHAT THE 3s IS FOR. While the blocker holds ACCESS
	// EXCLUSIVE, a plain SELECT — ACCESS SHARE, which conflicts — cannot run
	// either, and now fails at lock_timeout with the same 55P03 rather than
	// hanging for 30s. That is the second half of the argument in store.go's
	// comment: a lock wait on `orders` is not only Core's problem, it is every
	// other client of a shared plant database's problem too.
	var blocked int64
	if err := db.QueryRow(`SELECT count(*) FROM orders`).Scan(&blocked); err == nil {
		t.Error("SELECT succeeded while ACCESS EXCLUSIVE was held — the blocker did not hold its lock")
	} else {
		var readErr *pgconn.PgError
		if !errors.As(err, &readErr) || readErr.Code != "55P03" {
			t.Errorf("blocked SELECT failed with %v, want 55P03 lock_not_available", err)
		}
	}

	// Released, the same read works. Nothing about the aborted waiter is
	// sticky: no schema changed, no transaction is left holding anything.
	if err := held.Rollback(); err != nil {
		t.Fatalf("release blocker: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM orders`).Scan(&blocked); err != nil {
		t.Errorf("orders not readable after the blocker released: %v", err)
	}
}
