package bins

import (
	"strings"
	"testing"
)

// TestPreferredEmptyOrder_NoWantIsToday: a finder asked with no height runs
// today's ORDER BY, byte for byte, and binds no argument; asked with one, it
// binds exactly one, ranks the height ahead of today's terms and keeps them.
func TestPreferredEmptyOrder_NoWantIsToday(t *testing.T) {
	t.Parallel()
	for range 2 {
		a := &emptyQueryArgs{vals: []any{"T", int64(1)}}
		if got := preferredEmptyOrder(a, Height{}); got != AccessibleEmptyOrder {
			t.Errorf("no want: got\n%q\nwant today's\n%q", got, AccessibleEmptyOrder)
		}
		if len(a.vals) != 2 {
			t.Errorf("no want bound %d arguments, want the caller's 2", len(a.vals))
		}
	}

	five := Layers(5)
	a := &emptyQueryArgs{vals: []any{"T", int64(1)}}
	got := preferredEmptyOrder(a, five)
	again := preferredEmptyOrder(&emptyQueryArgs{vals: []any{"T", int64(1)}}, five)
	if got != again {
		t.Errorf("the same want rendered two texts:\n%q\n%q", got, again)
	}
	if len(a.vals) != 3 || a.vals[2] != 5 {
		t.Fatalf("a want bound %v, want the caller's two and then 5", a.vals)
	}
	if !strings.Contains(got, "($3::int * bt.totes_per_layer)") {
		t.Errorf("the want is not compared in totes of the candidate's type at $3:\n%s", got)
	}
	if !strings.HasSuffix(got, accessibleEmptyTerms+"\n\tLIMIT 1") {
		t.Errorf("today's terms do not follow the height:\n%s", got)
	}
}
