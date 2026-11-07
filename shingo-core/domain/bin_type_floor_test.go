package domain

import "testing"

// TestFloorSize_StringEqualsExactlyWhenTheSizesDo: the wire spelling keeps the
// floor rule's comparison, orientation-blind and with no rounding.
func TestFloorSize_StringEqualsExactlyWhenTheSizesDo(t *testing.T) {
	t.Parallel()
	floor := func(w, l float64) Footprint {
		fs, ok := BinType{WidthIn: w, LengthIn: l}.Footprint()
		if !ok {
			t.Fatalf("%gx%g has no footprint", w, l)
		}
		return fs
	}
	if got := floor(48, 45).String(); got != "45x48" {
		t.Errorf("48x45 = %q, want 45x48", got)
	}
	for _, c := range []struct{ a, b Footprint }{
		{floor(45, 48), floor(48, 45)},
		{floor(45, 48), floor(45, 48.5)},
		{floor(45.25, 48), floor(45.5, 48)},
		{floor(1000000, 48), floor(1000000.5, 48)},
	} {
		if (c.a == c.b) != (c.a.String() == c.b.String()) {
			t.Errorf("%v == %v is %v, but the strings %q and %q disagree", c.a, c.b, c.a == c.b, c.a.String(), c.b.String())
		}
	}
}
