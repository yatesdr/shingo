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

func TestOrderIsTotal(t *testing.T) {
	// Neither side may return a different arrangement from the other for the
	// same input, so no two distinct windows may compare equal in both
	// directions. SMN_03 and SMN_3 are the interesting pair: the same number,
	// different names.
	pairs := [][2]Window{
		{{Name: "SMN_03"}, {Name: "SMN_3"}},
		{{Ordinal: 1, Name: "A"}, {Ordinal: 1, Name: "A"}},
		{{Name: "W1"}, {Name: "W1X"}},
	}
	for _, p := range pairs {
		ab, ba := Less(p[0], p[1]), Less(p[1], p[0])
		if ab && ba {
			t.Errorf("%q and %q each sort before the other — the order is not total and the "+
				"two sides can disagree", p[0].Name, p[1].Name)
		}
		if p[0] == p[1] && (ab || ba) {
			t.Errorf("%q sorts before itself", p[0].Name)
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
