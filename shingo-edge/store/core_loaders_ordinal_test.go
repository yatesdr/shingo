package store

import (
	"testing"

	"shingo/protocol"
)

// The operator's arrangement had to survive four hops to matter: an admin
// screen that persists it, a wire that carries it, a cache column that stores
// it, and a read that honours it. Three of the four existed. The wire had no
// ordinal field and the cache had no ordinal column, so the read sorted by name
// and the arrangement was accepted, stored, transmitted and thrown away.
//
// These tests are the last two hops.

func TestCachedWindowsComeBackInTheOperatorArrangement(t *testing.T) {
	db := testDB(t)

	if err := db.ReplaceCoreLoaders([]protocol.LoaderInfo{{
		LoaderKey: "loader:1", Role: "produce", Name: "L1",
		Layout: "shared_window", Replenishment: "threshold",
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "W-A", Kind: "window", Ordinal: 3},
			{CoreNodeName: "W-Z", Kind: "window", Ordinal: 1},
			{CoreNodeName: "W-M", Kind: "window", Ordinal: 2},
		},
	}}); err != nil {
		t.Fatalf("write the cache: %v", err)
	}

	l, err := db.GetCoreLoader("loader:1")
	if err != nil || l == nil {
		t.Fatalf("read the cache: %v", err)
	}
	assertWindows(t, l.Positions, []string{"W-Z", "W-M", "W-A"},
		"the operator dragged the windows into this order and every layer above here "+
			"treats the first one as the funnel target and fills the rest in sequence")
}

func TestCachedWindowsFallBackToNaturalNameOrder(t *testing.T) {
	db := testDB(t)

	// No ordinals: nothing was ever dragged, and it is also exactly what a Core
	// that predates the field sends. The two cases are indistinguishable here and
	// must behave identically — this is the mixed-version story.
	if err := db.ReplaceCoreLoaders([]protocol.LoaderInfo{{
		LoaderKey: "loader:2", Role: "produce", Name: "L2",
		Layout: "shared_window", Replenishment: "threshold",
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "W10", Kind: "window"},
			{CoreNodeName: "W2", Kind: "window"},
			{CoreNodeName: "W1", Kind: "window"},
		},
	}}); err != nil {
		t.Fatalf("write the cache: %v", err)
	}

	l, err := db.GetCoreLoader("loader:2")
	if err != nil || l == nil {
		t.Fatalf("read the cache: %v", err)
	}
	assertWindows(t, l.Positions, []string{"W1", "W2", "W10"},
		"with nothing arranged the order comes from the names, and it has to be "+
			"number-aware — a plant naming windows W1..W9 sees nothing wrong until it "+
			"adds a tenth, at which point plain text puts W10 second and the funnel "+
			"target moves with nobody having touched anything")
}

func assertWindows(t *testing.T, got []CoreLoaderPosition, want []string, why string) {
	t.Helper()
	names := make([]string, len(got))
	for i, p := range got {
		names[i] = p.PositionNode
	}
	if len(names) != len(want) {
		t.Fatalf("windows = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("windows = %v, want %v — %s", names, want, why)
		}
	}
}

// THE CARRIER MIX HAS TO SURVIVE THE TRIP. It is declared on Core, carried on
// the wire, and read from the Edge cache — the same four hops the operator's
// window arrangement had to make, and that one was accepted, stored,
// transmitted and thrown away at the last of them.
//
// Two facts, and they answer different questions. The QUOTA is intent and
// belongs to the loader: how many carriers of each type it wants on hand. The
// CAPABILITY is physical and belongs to the window: what that slot can take.
// Empty capability means the window takes anything.
func TestCarrierMixSurvivesTheSync(t *testing.T) {
	db := testDB(t)

	if err := db.ReplaceCoreLoaders([]protocol.LoaderInfo{{
		LoaderKey: "loader:9", Role: "produce", Name: "MIX",
		Layout: "shared_window", Replenishment: "operator",
		Positions: []protocol.LoaderPosition{
			// A window that only fits one type, and one that fits two.
			{CoreNodeName: "W1", Kind: "window", Ordinal: 0, BinTypes: []string{"32x32"}},
			{CoreNodeName: "W2", Kind: "window", Ordinal: 1, BinTypes: []string{"45x48", "45x48-TOTE"}},
			// And one with nothing declared — takes anything.
			{CoreNodeName: "W3", Kind: "window", Ordinal: 2},
		},
		Quota: []protocol.LoaderQuota{
			{BinTypeCode: "45x48", Want: 3},
			{BinTypeCode: "32x32", Want: 1},
			{BinTypeCode: "45x48-TOTE", Want: 1},
		},
	}}); err != nil {
		t.Fatalf("write the cache: %v", err)
	}

	l, err := db.GetCoreLoader("loader:9")
	if err != nil || l == nil {
		t.Fatalf("read the cache: %v", err)
	}

	if len(l.Quota) != 3 {
		t.Fatalf("quota lines = %d, want 3 — the declared mix did not survive the trip", len(l.Quota))
	}
	want := map[string]int{"32x32": 1, "45x48": 3, "45x48-TOTE": 1}
	for _, q := range l.Quota {
		if want[q.BinTypeCode] != q.Want {
			t.Errorf("quota %s = %d, want %d", q.BinTypeCode, q.Want, want[q.BinTypeCode])
		}
	}

	byNode := map[string][]string{}
	for _, p := range l.Positions {
		byNode[p.PositionNode] = p.BinTypes
	}
	if got := byNode["W1"]; len(got) != 1 || got[0] != "32x32" {
		t.Errorf("W1 capability = %v, want [32x32]", got)
	}
	if got := byNode["W2"]; len(got) != 2 {
		t.Errorf("W2 capability = %v, want two types — a slot can fit more than one, "+
			"which is why capability is a set and not a column", got)
	}
	if got := byNode["W3"]; len(got) != 0 {
		t.Errorf("W3 capability = %v, want none — an undeclared window takes anything, "+
			"and that has to stay the meaning of empty or every existing loader "+
			"would suddenly accept nothing", got)
	}
}
