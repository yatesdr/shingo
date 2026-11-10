//go:build docker

package service

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/loaders"
)

// TestLoaderService_BoardSettings pins the board's two display settings: a new
// station gets what its board did when both followed replenishment (the rule
// v157 backfilled), an update stores them without touching replenishment, and
// the downward sync carries them to the Edge.
func TestLoaderService_BoardSettings(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)

	thresholdID, err := svc.Create("BS-THRESHOLD", loaders.RoleProduce, loaders.LayoutSharedWindow, "", "", "", false)
	testutil.MustNoErr(t, err, "create threshold loader")
	operatorID, err := svc.Create("BS-OPERATOR", loaders.RoleProduce, loaders.LayoutDedicatedPositions,
		loaders.ReplenishmentOperator, "", "", false)
	testutil.MustNoErr(t, err, "create operator-ordered loader")
	unloaderID, err := svc.Create("BS-UNLOADER", loaders.RoleConsume, loaders.LayoutSharedWindow, "", "", "", false)
	testutil.MustNoErr(t, err, "create unloader")

	for _, c := range []struct {
		id                  int64
		name                string
		hidesIdle, showsAll bool
	}{
		{thresholdID, "threshold loader", true, false},
		{operatorID, "operator-ordered loader", false, true},
		{unloaderID, "unloader", false, true},
	} {
		got, err := db.GetLoader(c.id)
		testutil.MustNoErr(t, err, "read "+c.name)
		if got.BoardHidesIdle != c.hidesIdle || got.BoardShowsAllParts != c.showsAll {
			t.Errorf("new %s: hides_idle=%v shows_all_parts=%v, want %v %v",
				c.name, got.BoardHidesIdle, got.BoardShowsAllParts, c.hidesIdle, c.showsAll)
		}
	}

	// The Hopkinsville case: operator-ordered, dedicated, set to hide.
	testutil.MustNoErr(t, svc.Update(LoaderUpdate{ID: operatorID, Name: "BS-OPERATOR",
		Layout: loaders.LayoutDedicatedPositions, BoardHidesIdle: true, BoardShowsAllParts: true}),
		"tick hide on the operator-ordered loader")
	got, err := db.GetLoader(operatorID)
	testutil.MustNoErr(t, err, "re-read operator-ordered loader")
	if !got.BoardHidesIdle || !got.BoardShowsAllParts || got.Replenishment != loaders.ReplenishmentOperator {
		t.Fatalf("after the update: %+v; want both board settings on and replenishment still operator", got)
	}

	infos, err := db.BuildLoaderInfos()
	testutil.MustNoErr(t, err, "build loader infos")
	found := false
	for _, li := range infos {
		if li.Name != "BS-OPERATOR" {
			continue
		}
		found = true
		if !li.BoardHidesIdle || !li.BoardShowsAllParts {
			t.Errorf("sync to the Edge: hides_idle=%v shows_all_parts=%v, want both true", li.BoardHidesIdle, li.BoardShowsAllParts)
		}
	}
	if !found {
		t.Fatal("BS-OPERATOR missing from the downward sync")
	}
}

// TestLoaderService_ClearPartials pins the save path of Clear partials: a new
// station keeps the clear, a save that switches it off stores it beside the
// other settings and the downward sync carries it to the Edge, and a save that
// omits it (the update is a full-row write) switches it back on.
func TestLoaderService_ClearPartials(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)
	id, err := svc.Create("CP-HOMES", loaders.RoleProduce, loaders.LayoutDedicatedPositions,
		loaders.ReplenishmentOperator, "", "", false)
	testutil.MustNoErr(t, err, "create the dedicated loader")
	synced := func() bool {
		t.Helper()
		infos, err := db.BuildLoaderInfos()
		testutil.MustNoErr(t, err, "build loader infos")
		for _, li := range infos {
			if li.Name == "CP-HOMES" {
				return li.NoClearPartials
			}
		}
		t.Fatal("CP-HOMES missing from the downward sync")
		return false
	}
	got, err := db.GetLoader(id)
	testutil.MustNoErr(t, err, "read the new loader")
	if got.NoClearPartials || synced() {
		t.Fatalf("a new station: no_clear_partials=%v, synced=%v; want the clear on", got.NoClearPartials, synced())
	}

	testutil.MustNoErr(t, svc.Update(LoaderUpdate{ID: id, Name: "CP-HOMES", Layout: loaders.LayoutDedicatedPositions,
		BoardHidesIdle: true, NoClearPartials: true}), "switch Clear partials off")
	got, err = db.GetLoader(id)
	testutil.MustNoErr(t, err, "re-read")
	if !got.NoClearPartials || !got.BoardHidesIdle || got.Replenishment != loaders.ReplenishmentOperator {
		t.Fatalf("after the save: %+v; want Clear partials off beside hide-idle, replenishment still operator", got)
	}
	if !synced() {
		t.Error("switched off, the sync to the Edge still says the clear is on")
	}

	testutil.MustNoErr(t, svc.Update(LoaderUpdate{ID: id, Name: "CP-HOMES", Layout: loaders.LayoutDedicatedPositions}),
		"a save that omits it")
	if got, err = db.GetLoader(id); err != nil || got.NoClearPartials {
		t.Errorf("a save without the field: no_clear_partials=%v (%v), want false", got.NoClearPartials, err)
	}
}
