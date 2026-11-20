//go:build docker

package service

import (
	"errors"
	"strings"
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

// TestLoaderService_EdgeStationUID pins the save path of a station's Edge: a
// new station picks none; a save naming an enrolled Edge stores its uid,
// trimmed, and the downward sync carries it; a uid Core has no Edge for is
// refused with words a person can read, and the stored pick stays; a save that
// omits it (the update is a full-row write) picks none again. A uid already
// stored is saved back unchecked, so a station whose Edge has left the
// registry still saves its other settings.
func TestLoaderService_EdgeStationUID(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)
	_, err := db.EnrollEdge("stn-esu-press", "PRESS EDGE")
	testutil.MustNoErr(t, err, "enroll the Edge")
	id, err := svc.Create("ESU-STATION", loaders.RoleProduce, loaders.LayoutSharedWindow,
		loaders.ReplenishmentOperator, "", "", false)
	testutil.MustNoErr(t, err, "create the loader")
	stored := func() string {
		t.Helper()
		got, err := db.GetLoader(id)
		testutil.MustNoErr(t, err, "read the loader")
		return got.StationUID
	}
	synced := func() string {
		t.Helper()
		infos, err := db.BuildLoaderInfos()
		testutil.MustNoErr(t, err, "build loader infos")
		for _, li := range infos {
			if li.Name == "ESU-STATION" {
				return li.EdgeStationUID
			}
		}
		t.Fatal("ESU-STATION missing from the downward sync")
		return ""
	}
	if stored() != "" || synced() != "" {
		t.Fatalf("a new station: stored %q, synced %q; want no Edge picked", stored(), synced())
	}

	testutil.MustNoErr(t, svc.Update(LoaderUpdate{ID: id, Name: "ESU-STATION", BoardHidesIdle: true,
		StationUID: "  stn-esu-press "}), "pick the enrolled Edge")
	if stored() != "stn-esu-press" || synced() != "stn-esu-press" {
		t.Fatalf("picked: stored %q, synced %q; want stn-esu-press, trimmed", stored(), synced())
	}

	err = svc.Update(LoaderUpdate{ID: id, Name: "ESU-STATION", StationUID: "stn-esu-nobody"})
	if !errors.Is(err, ErrUnknownEdge) {
		t.Fatalf("a uid with no Edge: err = %v, want ErrUnknownEdge so the door answers 400", err)
	}
	if !strings.Contains(err.Error(), `no Edge with station uid "stn-esu-nobody"`) {
		t.Errorf("refusal reads %q; want it to name the uid", err.Error())
	}
	if stored() != "stn-esu-press" {
		t.Errorf("after the refusal: stored %q, want the pick before it", stored())
	}

	testutil.MustNoErr(t, svc.Update(LoaderUpdate{ID: id, Name: "ESU-STATION"}), "a save that omits it")
	if stored() != "" || synced() != "" {
		t.Errorf("a save without the field: stored %q, synced %q; want no Edge picked", stored(), synced())
	}

	// An Edge that has left the registry: the uid stays on the loader, and a
	// save of another setting that sends it back is not refused.
	cur, err := db.GetLoader(id)
	testutil.MustNoErr(t, err, "read for the direct write")
	cur.StationUID = "stn-esu-gone"
	testutil.MustNoErr(t, db.UpdateLoader(*cur), "store a uid the registry does not hold")
	testutil.MustNoErr(t, svc.Update(LoaderUpdate{ID: id, Name: "ESU-STATION", BoardShowsAllParts: true,
		StationUID: "stn-esu-gone"}), "save another setting with the stored uid sent back")
	if got, err := db.GetLoader(id); err != nil || got.StationUID != "stn-esu-gone" || !got.BoardShowsAllParts {
		t.Errorf("after the save: %+v (%v); want the stored uid kept beside the new setting", got, err)
	}
}
