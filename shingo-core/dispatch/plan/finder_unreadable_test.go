package plan

import (
	"database/sql"
	"errors"
	"testing"
)

// finder_unreadable_test.go — sourceSearchFailed, the picker's "did the search
// run" classifier, moved into the planner with the picker. Its four call-site
// tests stay in dispatch (source_finder_unreadable_test.go), where the fake
// store they drive lives.

// sourceSearchFailed is the one place the distinction is decided, so it gets a
// direct test as well as the four call-site ones in dispatch.
//
// THE WRAPPED CASE IS THE ONE THAT MATTERS. The store wraps with %w in some
// paths and returns the sentinel bare in others; a classifier using == rather
// than errors.Is would read a wrapped none-found as a failure, which is the
// same collapse pointing the other way.
func TestSourceSearchFailed_ClassifiesTheThreeCases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil — neither a failure nor a match", nil, false},
		{"bare sql.ErrNoRows — the query ran, nothing matched", sql.ErrNoRows, false},
		{"WRAPPED sql.ErrNoRows — still none-found", errNoRowsWrapped(), false},
		{"a real error — the query did not run", errors.New("connection refused"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceSearchFailed(tc.err); got != tc.want {
				t.Errorf("sourceSearchFailed(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func errNoRowsWrapped() error {
	return &wrapped{msg: "count empty 45x58 in group 12", err: sql.ErrNoRows}
}

type wrapped struct {
	msg string
	err error
}

func (w *wrapped) Error() string { return w.msg + ": " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }
