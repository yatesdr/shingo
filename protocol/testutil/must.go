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

// Must returns v, failing the test if err is non-nil. Same message
// convention as MustNoErr ("msg: %v"); this is the two-value form, for
// the lookups whose result the test then reads.
//
// It exists because of what the untyped alternative does to a failed
// run. The shape it replaces is
//
//	order, _ := db.GetOrder(id)
//	if order.Status != ... {
//
// which, when GetOrder fails, does not report the failure — it reports
// a nil pointer dereference on the next line. Under a saturated docker
// suite that is the common case, not the rare one: postgres refuses a
// connection, or a SASL handshake times out, and thirty tests panic
// somewhere downstream of the query that actually failed. The panic
// trace names the dereference, the goroutine dump buries the rest of
// the log, and the one line that said WHY — the pgx error — was thrown
// away at the `_`. A run that destroys its own evidence costs more than
// the run it replaced, because the next step is to reproduce it.
//
// Must names the cause on the line that has it:
//
//	got, err := db.GetOrder(id)
//	order := Must(t, got, err, "reload order")
//
// Note the two lines. Go will not let a multi-value call be spread into
// a parameter list that has other arguments, so Must(t, db.GetOrder(id),
// "reload order") does not compile and no signature carrying t and a
// message can make it. Where the value is not read, MustNoErr on the
// error alone stays the shorter form.
func Must[T any](t *testing.T, v T, err error, msg string) T {
	t.Helper()
	MustNoErr(t, err, msg)
	return v
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
