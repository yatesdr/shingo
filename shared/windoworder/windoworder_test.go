package windoworder

import (
	"sort"
	"testing"
)

func TestOperatorArrangementWins(t *testing.T) {
	got := sorted([]Window{
		{Ordinal: 2, Name: "W-A"},
		{Ordinal: 0, Name: "W-Z"},
		{Ordinal: 1, Name: "W-M"},
	})
	want := []string{"W-Z", "W-M", "W-A"}
	assertOrder(t, got, want,
		"the operator dragged these into this order and the system accepted, stored, "+
			"and transmitted the arrangement; sorting by name here is what threw it away")
}

func TestFallsBackToNameWhenNothingWasArranged(t *testing.T) {
	// Every ordinal zero is what a Core that predates the field sends, and it
	// is also a loader nobody has ever dragged. Both mean "no arrangement".
	got := sorted([]Window{
		{Name: "W-Z"}, {Name: "W-A"}, {Name: "W-M"},
	})
	assertOrder(t, got, []string{"W-A", "W-M", "W-Z"},
		"with no arrangement to honour the order has to come from somewhere stable")
}

func TestTenWindowsDoNotReorderThemselves(t *testing.T) {
	// The trap the name fallback exists to avoid. A plant names its windows W1,
	// W2, W3 and plain text order matches intent exactly until the loader
	// reaches ten — at which point W10 sorts before W2 and the funnel target
	// moves with nobody having touched anything.
	got := sorted([]Window{
		{Name: "W10"}, {Name: "W2"}, {Name: "W1"}, {Name: "W20"}, {Name: "W3"},
	})
	assertOrder(t, got, []string{"W1", "W2", "W3", "W10", "W20"},
		"a digit run inside a name has to compare as a number")
}

func TestZeroPaddedPlantNamesSortNaturally(t *testing.T) {
	// The real shape at both plants: SMN_001..SMN_004, PLN_01..PLN_12.
	got := sorted([]Window{
		{Name: "SMN_004"}, {Name: "SMN_001"}, {Name: "SMN_010"}, {Name: "SMN_002"},
	})
	assertOrder(t, got, []string{"SMN_001", "SMN_002", "SMN_004", "SMN_010"},
		"leading zeros must not change a number's value")
}

// TestOrderIsAStrictWeakOrdering asserts what the comparator actually
// guarantees, which is what sort.SliceStable needs: nothing sorts before
// itself, and no two things sort before each other.
//
// It was called TestOrderIsTotal and it asserted the same `ab && ba` check.
// That check cannot detect the failure it was named for. A comparator breaks
// totality by returning false BOTH ways — the two are equal without being
// identical — and `ab && ba` is false in exactly that case, so the test passed.
// It listed SMN_03/SMN_3 as "the interesting pair" and was green on it.
func TestOrderIsAStrictWeakOrdering(t *testing.T) {
	pairs := [][2]Window{
		{{Name: "SMN_03"}, {Name: "SMN_3"}},
		{{Ordinal: 1, Name: "A"}, {Ordinal: 1, Name: "A"}},
		{{Name: "W1"}, {Name: "W1X"}},
		{{Name: "W2"}, {Name: "W10"}},
		{{Name: ""}, {Name: "W1"}},
	}
	for _, p := range pairs {
		ab, ba := Less(p[0], p[1]), Less(p[1], p[0])
		if ab && ba {
			t.Errorf("%q and %q each sort before the other — sort.SliceStable's contract is "+
				"broken and the arrangement is undefined", p[0].Name, p[1].Name)
		}
		if p[0] == p[1] && (ab || ba) {
			t.Errorf("%q sorts before itself", p[0].Name)
		}
	}
}

// TestLeadingZeroNamesCompareEqual pins the gap the package doc describes, so
// that it stays a known deferral rather than becoming a surprise.
//
// These names are DISTINCT and compare EQUAL. Under a stable sort equal
// elements keep their input order, and Core and the Edge do not build the same
// input — Core reads ORDER BY sort_order, position_node_id, the Edge reads its
// cache unordered. So a loader carrying one of these pairs can have the two
// sides disagree about which window is first.
//
// IF YOU ARE HERE BECAUSE THIS TEST FAILED: you probably added the plain-text
// tiebreak, which is the right fix. It is also a behaviour change to the rule
// both modules import — delivery moves at any plant holding such a pair — so
// re-run the loader parity harness and check the plant configuration before
// shipping it, then update this test to assert the new ordering.
func TestLeadingZeroNamesCompareEqual(t *testing.T) {
	pairs := [][2]string{
		{"SMN_03", "SMN_3"},
		{"W02", "W2"},
		{"W007", "W7"},
	}
	for _, p := range pairs {
		ab, ba := LessName(p[0], p[1]), LessName(p[1], p[0])
		if ab || ba {
			t.Errorf("LessName(%q,%q)=%v and LessName(%q,%q)=%v — these used to compare equal; "+
				"if that was fixed deliberately, see this test's doc comment before trusting it",
				p[0], p[1], ab, p[1], p[0], ba)
		}
	}
}

func sorted(ws []Window) []Window {
	out := append([]Window(nil), ws...)
	sort.SliceStable(out, func(i, j int) bool { return Less(out[i], out[j]) })
	return out
}

func assertOrder(t *testing.T, got []Window, want []string, why string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d windows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i] {
			names := make([]string, len(got))
			for k, w := range got {
				names[k] = w.Name
			}
			t.Fatalf("order = %v, want %v — %s", names, want, why)
		}
	}
}
