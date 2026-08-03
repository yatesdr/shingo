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
