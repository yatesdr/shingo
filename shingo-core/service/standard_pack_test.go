package service

import (
	"errors"
	"testing"
)

// Pure (no Postgres): the standard-pack rule every template load reads. No
// declaration is the pack; a declared 0 is an empty bin; a declared count above
// the pack is kept; a declared negative is refused (owner, 2026-11-19).
func TestStandardPackUOP(t *testing.T) {
	t.Parallel()
	n := func(v int) *int { return &v }
	for _, c := range []struct {
		name     string
		declared *int
		want     int
	}{
		{"undeclared is the pack", nil, 800},
		{"0 is empty", n(0), 0},
		{"a partial is kept", n(404), 404},
		{"above the pack is kept", n(803), 803},
	} {
		got, err := standardPackUOP(800, c.declared)
		if err != nil || got != c.want {
			t.Errorf("%s: standardPackUOP(800) = %d, %v; want %d", c.name, got, err, c.want)
		}
	}
	if got, err := standardPackUOP(800, n(-5)); !errors.Is(err, ErrNegativeCount) {
		t.Errorf("a declared -5 = %d, %v; want ErrNegativeCount", got, err)
	}
}
