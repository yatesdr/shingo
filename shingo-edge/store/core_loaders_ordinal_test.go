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
			{CoreNodeName: "W-A", Ordinal: 3},
			{CoreNodeName: "W-Z", Ordinal: 1},
			{CoreNodeName: "W-M", Ordinal: 2},
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
			{CoreNodeName: "W10"},
			{CoreNodeName: "W2"},
			{CoreNodeName: "W1"},
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

// TestTheSyncCachesNoFactTheEdgeDoesNotRead pins what the loader sync leaves out
// of the cache: the loader's config generation. Nothing on the Edge reads it,
// so the sync leaves core_loaders.config_gen at its default. (Each position's
// kind and the carrier mix have no place in the schema since Edge v27:
// loader_wire_drop_migration_test.go.)
func TestTheSyncCachesNoFactTheEdgeDoesNotRead(t *testing.T) {
	db := testDB(t)

	if err := db.ReplaceCoreLoaders([]protocol.LoaderInfo{{
		LoaderKey: "loader:9", Role: "produce", Name: "MIX",
		Layout: "shared_window", Replenishment: "operator",
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "W1", Ordinal: 0},
			{CoreNodeName: "W2", Ordinal: 1},
		},
		Payloads: []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}},
	}}); err != nil {
		t.Fatalf("write the cache: %v", err)
	}

	var configGen int
	if err := db.QueryRow(`SELECT config_gen FROM core_loaders WHERE loader_key = 'loader:9'`).Scan(&configGen); err != nil {
		t.Fatalf("read config_gen: %v", err)
	}
	if configGen != 0 {
		t.Errorf("cached config_gen = %d, want 0: the Edge reads none of it", configGen)
	}
	l, err := db.GetCoreLoader("loader:9")
	if err != nil || l == nil || len(l.Positions) != 2 || len(l.Payloads) != 1 {
		t.Fatalf("read the cache = %+v, %v; want the loader with its two windows and one payload", l, err)
	}
}
