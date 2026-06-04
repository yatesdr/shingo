package service

import "testing"

// TestLinesideStarved pins the floor-based starvation danger tier (task 2):
// a manual_swap loader payload is starved when lineside UOP drops below a
// quarter of a full bin. Unknown capacity never trips (no floor).
func TestLinesideStarved(t *testing.T) {
	cases := []struct {
		name     string
		capacity int
		lineside int
		want     bool
	}{
		{"below quarter is starved", 200, 49, true},    // floor = 50
		{"exactly at quarter is safe", 200, 50, false}, // not strictly below
		{"well above quarter is safe", 200, 120, false},
		{"empty is starved", 200, 0, true},
		{"unknown capacity never trips", 0, 0, false},
		{"negative capacity never trips", -1, 5, false},
	}
	for _, c := range cases {
		if got := linesideStarved(c.capacity, c.lineside); got != c.want {
			t.Errorf("%s: linesideStarved(%d, %d) = %v, want %v", c.name, c.capacity, c.lineside, got, c.want)
		}
	}
}
