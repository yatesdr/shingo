package kanban

import "testing"

func TestLevel_Want(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		l    Level
		want int
	}{
		{"nothing declared, nothing on the line", Level{}, 0},
		{"no positions in the type", Level{Base: 2, Cover: 0}, 2},
		{"four positions", Level{Base: 1, Cover: 4}, 5},
		{"base 0 lifted by the line", Level{Base: 0, Cover: 4}, 4},
	} {
		if got := tc.l.Want(); got != tc.want {
			t.Errorf("%s: Want() = %d, want %d", tc.name, got, tc.want)
		}
	}
}
