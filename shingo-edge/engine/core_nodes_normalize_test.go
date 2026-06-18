package engine

import "testing"

// TestBareNodeName pins the group-qualifier trim: Core sends group children as
// "Group.Child" for display, but the runtime keys on the bare child name, so the
// edge must reduce to the segment after the last dot (and leave bare/top-level
// names untouched).
func TestBareNodeName(t *testing.T) {
	cases := map[string]string{
		"Supermarket Area.SMN_01":        "SMN_01",
		"Supermarket Empty Totes.SMN_05": "SMN_05",
		"SMN_01":                         "SMN_01", // already bare
		"PLN_01":                         "PLN_01", // top-level, no group
		"":                               "",
	}
	for in, want := range cases {
		if got := bareNodeName(in); got != want {
			t.Errorf("bareNodeName(%q) = %q, want %q", in, want, got)
		}
	}
}
