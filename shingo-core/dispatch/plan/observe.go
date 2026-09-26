package plan

import "sync/atomic"

// observe.go — the seam the zero-write test watches the planner through.
//
// THE PLANNER IS PURE, AND THAT IS PROVED THREE WAYS (B1): by type (a part takes
// only the read-only Reader), by lint (the planner-purity depguard rule), and by
// measurement. The measurement runs every planner golden case against a
// statement-counting store and asserts that no statement issued inside a
// planner call writes, locks, or opens a transaction
// (cmd/seeddev/planner_zero_write_docker_test.go). To know where a planner call
// begins and ends when a door makes it, the test needs to be told; this is how.
//
// TEST INSTRUMENT, NIL IN PRODUCTION. Nothing sets an Observer outside that
// test. With none set, observe is one atomic load and returns a shared no-op, so
// a planner call costs nothing it did not cost before, and no statement.

// Observer is told when a planner part is entered and left. db is the reader the
// part reads through, which is how a test running cases in parallel tells its
// own calls from its neighbours'; part names the entry point. Calls nest (a
// complex step resolution picks, and picks resolve groups).
type Observer func(db any, part string, enter bool)

var observer atomic.Pointer[Observer]

// SetObserver installs o, or removes the observer when o is nil. For the
// zero-write test only.
func SetObserver(o Observer) {
	if o == nil {
		observer.Store(nil)
		return
	}
	observer.Store(&o)
}

func noLeave() {}

// observe reports entering part and returns the call that reports leaving it:
// `defer observe(db, "pick")()`.
func observe(db any, part string) func() {
	o := observer.Load()
	if o == nil {
		return noLeave
	}
	(*o)(db, part, true)
	return func() { (*o)(db, part, false) }
}
