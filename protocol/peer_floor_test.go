package protocol

import "testing"

// TestCompareToFloor_EveryShapeAStampCanTake: every version shape the
// installers and the dev builds stamp, against both floors. A release compares
// by number, not by text (v0.9.3 is below v0.17.2, which a string compare gets
// wrong); a build past a tag stands at that tag; -dirty changes nothing; what
// names no release is unknown, never below and never at or above.
func TestCompareToFloor_EveryShapeAStampCanTake(t *testing.T) {
	t.Parallel()
	for _, floor := range []string{FloorCore, FloorEdge} {
		if _, ok := parseRelease(floor); !ok {
			t.Fatalf("the floor %q names no release: every compare against it would read unknown", floor)
		}
	}
	for _, tc := range []struct {
		version, floor string
		want           FloorStanding
	}{
		{"v0.17.2", FloorEdge, FloorAtOrAbove},
		{"v0.17.1", FloorEdge, FloorBelow},
		{"v0.17.3", FloorEdge, FloorAtOrAbove},
		{"v0.18.0", FloorEdge, FloorAtOrAbove},
		{"v0.9.3", FloorEdge, FloorBelow},
		{"v0.16.10", FloorEdge, FloorBelow},
		{"v1.0.0", FloorEdge, FloorAtOrAbove},
		{"v0.17.2-dirty", FloorEdge, FloorAtOrAbove},
		{"v0.17.1-dirty", FloorEdge, FloorBelow},
		{"v0.17.2-3-gabc1234", FloorEdge, FloorAtOrAbove},
		{"v0.17.1-941-g7c21ef3f", FloorEdge, FloorBelow},
		{"v0.17.2-3-gabc1234-dirty", FloorEdge, FloorAtOrAbove},
		{"v0.18.0", FloorCore, FloorAtOrAbove},
		{"v0.17.2", FloorCore, FloorBelow},
		{"dev", FloorEdge, FloorUnknown},
		{"", FloorEdge, FloorUnknown},
		{"unknown", FloorCore, FloorUnknown},
		{"7c21ef3f", FloorEdge, FloorUnknown},
		{"7c21ef3f-dirty", FloorEdge, FloorUnknown},
		{"0.17.2", FloorEdge, FloorUnknown},
		{"v0.17", FloorEdge, FloorUnknown},
		{"v0.17.2-rc1", FloorEdge, FloorUnknown},
		{"v00.17.2", FloorEdge, FloorUnknown},
		{"v0.17.2", "dev", FloorUnknown},
	} {
		if got := CompareToFloor(tc.version, tc.floor); got != tc.want {
			t.Errorf("CompareToFloor(%q, %q) = %d, want %d", tc.version, tc.floor, got, tc.want)
		}
	}
}
