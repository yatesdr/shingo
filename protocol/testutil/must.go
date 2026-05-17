package testutil

import (
	"context"
	"testing"
	"time"
)

// MustNoErr fails the test if err is non-nil. The message is appended
// as a prefix to the error using "msg: %v" formatting. Use to collapse
// the repetitive `if err != nil { t.Fatalf(...) }` block:
//
//	testutil.MustNoErr(t, db.Insert(row), "insert row")
func MustNoErr(t *testing.T, err error, msg string) {
	t.Helper()
	if err != nil {
		if msg == "" {
			t.Fatalf("unexpected error: %v", err)
		} else {
			t.Fatalf("%s: %v", msg, err)
		}
	}
}

// Context returns a context that expires at the given timeout and
// is cancelled at test cleanup. Use to give async operations a
// per-test deadline — without this, flakes show up as CI-timeout
// hangs instead of clean failures.
func Context(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}
