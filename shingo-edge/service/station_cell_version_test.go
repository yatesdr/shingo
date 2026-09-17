package service

import (
	"context"
	"testing"

	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
)

// station_cell_version_test.go — every door that changes the cell picture,
// driven through the real write path, with the version read off a real poll.
//
// WHY THIS EXISTS BESIDE THE DOMAIN TEST. domain/cell_picture_version_test.go
// holds the FUNCTION: given these inputs, the string moves. It cannot see the
// half that actually breaks — whether the input reaches the function at all
// when somebody edits a cell. A writer that forgets to publish its generation,
// or a poll that reads a stale copy of the claims, leaves every board drawing a
// cell that is not there, with nothing saying so. So each door below is opened
// the way the product opens it, and the version is read the way a board reads
// it.

func versionFixture(t *testing.T) (*StationService, int64, testdb.SeededPlant) {
	t.Helper()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")
	stationID, ok := seeded.Stations["screen-a4"]
	if !ok {
		t.Fatalf("the press station is not seeded: %v", seeded.Stations)
	}
	svc := NewStationService(db)
	svc.SetBinTypeResolver(testdb.BinTypeOf(scenefixtures.A()))
	return svc, stationID, seeded
}

func pollVersion(t *testing.T, svc *StationService, stationID int64) string {
	t.Helper()
	view, err := svc.BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if view.CellVersion == "" {
		t.Fatal("a poll carried no cell-picture version")
	}
	return view.CellVersion
}

func TestCellVersion_MovesOnEveryDoorThroughTheRealPath(t *testing.T) {
	svc, stationID, seeded := versionFixture(t)
	db := svc.db

	// A POLL THAT CHANGES NOTHING CHANGES NOTHING. Asserted first, because it
	// is the half a "bump on everything" implementation would fail — and that
	// implementation sends every board to fetch its picture twice a second,
	// which is worse than what this replaced.
	before := pollVersion(t, svc, stationID)
	if again := pollVersion(t, svc, stationID); again != before {
		t.Fatalf("two polls over an unchanged cell gave %q and %q; every board refetches its picture "+
			"on every poll", before, again)
	}

	moved := func(door string) string {
		t.Helper()
		now := pollVersion(t, svc, stationID)
		if now == before {
			t.Errorf("%s: the version did not move. Every board holding %q goes on drawing the cell "+
				"as it was, and nothing says so", door, before)
		}
		before = now
		return now
	}

	styleID := seeded.Styles["PART SYN-A-S003"]
	if styleID == 0 {
		t.Fatal("the fixture's press-index style is not seeded")
	}

	// ── a cutover ────────────────────────────────────────────────────────
	if err := db.SetActiveStyle(seeded.ProcessID, &styleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	moved("a cutover (the running style changed)")

	// ── a flow save: the claim write every save path ends in ─────────────
	claims, err := db.ListStyleNodeClaims(styleID)
	if err != nil || len(claims) == 0 {
		t.Fatalf("list claims: %v (%d)", err, len(claims))
	}
	in := domain.InputFromClaim(claims[0])
	// A value the claim demonstrably does not already carry: a save that
	// writes back what was there is not a change, and a test that asserted the
	// version moved for one would be asserting the wrong thing.
	in.OutboundDestination = in.OutboundDestination + " Annex"
	if _, err := db.UpsertStyleNodeClaim(in); err != nil {
		t.Fatalf("flow save (claim upsert): %v", err)
	}
	moved("a flow save moved a claim's outbound destination")

	// ── a claim retired ──────────────────────────────────────────────────
	if err := db.DeleteStyleNodeClaim(claims[len(claims)-1].ID); err != nil {
		t.Fatalf("retire a claim: %v", err)
	}
	moved("a claim was deleted")

	// ── a station's node list is set ─────────────────────────────────────
	//
	// THE DOOR THAT NEEDS THE GENERATION COUNTER. Nothing about the claims
	// changes here; process_nodes does, and the poll does not read it.
	names, err := svc.GetNodeNames(stationID)
	if err != nil || len(names) < 2 {
		t.Fatalf("station node names: %v (%v)", err, names)
	}
	if err := svc.SetNodes(stationID, names[:len(names)-1]); err != nil {
		t.Fatalf("SetNodes: %v", err)
	}
	moved("a station's node list was set (a position left the cell)")

	// ── a changeover start that mints a position ─────────────────────────
	//
	// The other process_nodes door, and the one nobody would think of: a
	// changeover auto-creates a row for a claimed node the process has none
	// for, so STARTING one can change the cell's shape.
	co := NewChangeoverService(db)
	existing, err := db.ListProcessNodesByProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if _, err := co.Create(seeded.ProcessID, nil, styleID, "test", "",
		nil,
		[]processes.NodeTaskInput{{
			ProcessID: seeded.ProcessID, CoreNodeName: "PLN_MINTED",
			Situation: "swap", State: "swap_required",
		}}, nil, existing); err != nil {
		t.Fatalf("changeover create: %v", err)
	}
	moved("a changeover start minted a position")

	// ── the plant caches were replaced ───────────────────────────────────
	//
	// The geometry cache and the NGRP map are replaced together by a node-list
	// response and published on one counter; the service reads that counter
	// rather than either cache, because reading the group map is the deep copy
	// this whole change exists to take off the poll.
	gen := uint64(1)
	svc.SetPlantGenerationResolver(func() uint64 { return gen })
	before = pollVersion(t, svc, stationID)
	gen++
	moved("the geometry cache / group map was replaced")
}

// A SECOND STATION OF THE SAME CELL HAS ITS OWN PICTURE, so it must have its
// own version: one string for both would hand a board the other one's
// positions the first time they diverged.
func TestCellVersion_IsPerStation(t *testing.T) {
	svc, stationID, seeded := versionFixture(t)
	other := int64(0)
	for _, id := range seeded.Stations {
		if id != stationID {
			other = id
			break
		}
	}
	if other == 0 {
		t.Skip("the fixture seeds one station for this process")
	}
	if a, b := pollVersion(t, svc, stationID), pollVersion(t, svc, other); a == b {
		t.Errorf("two stations share the cell-picture version %q", a)
	}
}
