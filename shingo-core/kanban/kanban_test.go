package kanban

import "testing"

func TestLevel_Want(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		l    Level
		want int
	}{
		{"nothing declared, nothing running", Level{}, 0},
		{"cover 0 ignores what runs", Level{Base: 2, Cover: 0, Running: 3}, 2},
		{"nothing runs the type", Level{Base: 2, Cover: 4, Running: 0}, 2},
		{"one process running", Level{Base: 1, Cover: 4, Running: 1}, 5},
		{"two processes running", Level{Base: 1, Cover: 4, Running: 2}, 9},
		{"base 0 lifted by a running process", Level{Base: 0, Cover: 4, Running: 1}, 4},
	} {
		if got := tc.l.Want(); got != tc.want {
			t.Errorf("%s: Want() = %d, want %d", tc.name, got, tc.want)
		}
	}
}
