package plan

import "testing"

// TestDropPayload: a drop asks its group with its own payload; with none, it
// asks with what it carries — nothing for an empty, the order's for a full (C2b
// Q4). A full set down mid-route must stay fenced by the payload it carries.
func TestDropPayload(t *testing.T) {
	t.Parallel()
	// steps: 0 pickup, 1 drop, 2 pickup, 3 drop
	pickup := func(j int) bool { return j == 0 || j == 2 }
	for _, tc := range []struct {
		name        string
		i           int
		stepPayload string
		emptyAt     map[int]bool
		want        string
	}{
		{"an empty's drop asks with nothing", 1, "", map[int]bool{0: true}, ""},
		{"a full's drop asks with the order's", 1, "", nil, "ORDER"},
		{"the step's own payload wins", 1, "STEP", map[int]bool{0: true}, "STEP"},
		{"the NEAREST pickup decides", 3, "", map[int]bool{0: true}, "ORDER"},
		{"no pickup ahead: the order's", 0, "", nil, "ORDER"},
	} {
		got := DropPayload(tc.i, tc.stepPayload, "ORDER", pickup, func(j int) bool { return tc.emptyAt[j] })
		if got != tc.want {
			t.Errorf("%s: DropPayload = %q, want %q", tc.name, got, tc.want)
		}
	}
}
