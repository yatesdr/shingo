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
