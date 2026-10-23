//go:build docker

package nodes_test

import (
	"database/sql"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// mkBinType inserts a bin type and returns its id. The maintain tables key on
// bin_type_id with a real FK, so a level row needs one to exist.
func mkBinType(t *testing.T, sdb *sql.DB, code string) int64 {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, sdb.QueryRow(
		`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`, code).Scan(&id), "insert bin type")
	return id
}

// TestMaintainLevels_DeclareChangeUndeclare covers the three things an editor
// does to a level line, and the distinction the API exists to preserve:
// want=0 KEEPS the line ("none of this type right now"), removing it stops
// declaring the type at all. Collapsing those two would make "we deliberately
// hold none" and "we never said" the same row, which is the whole reason want
// carries a CHECK (want >= 0) instead of being deleted at zero.
func TestMaintainLevels_DeclareChangeUndeclare(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	grpID, err := nodes.CreateGroup(sdb, "GRP-MAINTAIN")
	testutil.MustNoErr(t, err, "CreateGroup")
	testdb.MarkGroup(t, db, grpID)
	big := mkBinType(t, sdb, "45x58x32")
	small := mkBinType(t, sdb, "45x48x24")

	// Nothing declared: empty, not an error.
	got, err := nodes.ListMaintainLevels(sdb, grpID)
	testutil.MustNoErr(t, err, "ListMaintainLevels empty")
	if len(got) != 0 {
		t.Fatalf("levels on a fresh group = %d, want 0", len(got))
	}

	testutil.MustNoErr(t, nodes.SetMaintainLevel(sdb,
		nodes.MaintainLevel{GroupNodeID: grpID, BinTypeID: big, Want: 4}), "declare big")
	testutil.MustNoErr(t, nodes.SetMaintainLevel(sdb,
		nodes.MaintainLevel{GroupNodeID: grpID, BinTypeID: small, Want: 2}), "declare small")

	// Ordered by CODE, and the code is joined on — the id is a local key, the
	// code is what a person reads.
	got, err = nodes.ListMaintainLevels(sdb, grpID)
	testutil.MustNoErr(t, err, "ListMaintainLevels")
	if len(got) != 2 {
		t.Fatalf("levels = %d, want 2", len(got))
	}
	if got[0].BinTypeCode != "45x48x24" || got[0].Want != 2 {
		t.Errorf("levels[0] = %+v, want 45x48x24 want=2", got[0])
	}
	if got[1].BinTypeCode != "45x58x32" || got[1].Want != 4 {
		t.Errorf("levels[1] = %+v, want 45x58x32 want=4", got[1])
	}

	// Upsert: re-declaring a type changes the number, it does not add a row.
	testutil.MustNoErr(t, nodes.SetMaintainLevel(sdb,
		nodes.MaintainLevel{GroupNodeID: grpID, BinTypeID: big, Want: 6}), "re-declare big")
	got, err = nodes.ListMaintainLevels(sdb, grpID)
	testutil.MustNoErr(t, err, "ListMaintainLevels after upsert")
	if len(got) != 2 {
		t.Fatalf("levels after upsert = %d, want 2 (upsert, not insert)", len(got))
	}
	if got[1].Want != 6 {
		t.Errorf("big want after upsert = %d, want 6", got[1].Want)
	}

	// want=0 KEEPS the line.
	testutil.MustNoErr(t, nodes.SetMaintainLevel(sdb,
		nodes.MaintainLevel{GroupNodeID: grpID, BinTypeID: small, Want: 0}), "zero small")
	got, err = nodes.ListMaintainLevels(sdb, grpID)
	testutil.MustNoErr(t, err, "ListMaintainLevels after zero")
	if len(got) != 2 {
		t.Fatalf("levels after want=0 = %d, want 2 — zero is a declared value, not a delete", len(got))
	}

	// Removing DROPS it.
	testutil.MustNoErr(t, nodes.RemoveMaintainLevel(sdb, grpID, small), "RemoveMaintainLevel")
	got, err = nodes.ListMaintainLevels(sdb, grpID)
	testutil.MustNoErr(t, err, "ListMaintainLevels after remove")
	if len(got) != 1 || got[0].BinTypeCode != "45x58x32" {
		t.Errorf("levels after remove = %+v, want only 45x58x32", got)
	}
}

// TestMaintainLevels_NegativeWantRefused pins the CHECK. A negative level is not
// a smaller level, it is a number no keeper can act on, and the database is the
// only place that can say so for every writer at once.
func TestMaintainLevels_NegativeWantRefused(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	grpID, err := nodes.CreateGroup(sdb, "GRP-NEGATIVE")
	testutil.MustNoErr(t, err, "CreateGroup")
	testdb.MarkGroup(t, db, grpID)
	btID := mkBinType(t, sdb, "NEG-TOTE")

	if err := nodes.SetMaintainLevel(sdb,
		nodes.MaintainLevel{GroupNodeID: grpID, BinTypeID: btID, Want: -1}); err == nil {
		t.Fatal("SetMaintainLevel(want=-1): expected the CHECK to refuse it, got nil")
	}
}

// TestMaintainSupports_ReplacesWholeSet covers the replace semantics. The editor
// holds the whole set on screen and sends the whole set; a second save with a
// narrower selection has to REMOVE what it dropped, or the screen and the table
// diverge silently and the group keeps serving a process nobody thinks it does.
func TestMaintainSupports_ReplacesWholeSet(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	grpID, err := nodes.CreateGroup(sdb, "GRP-SUPPORTS")
	testutil.MustNoErr(t, err, "CreateGroup")
	testdb.MarkGroup(t, db, grpID)

	pressA := &nodes.Node{Name: "PRESS-A", Enabled: true}
	pressB := &nodes.Node{Name: "PRESS-B", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, pressA), "create PRESS-A")
	testutil.MustNoErr(t, nodes.Create(sdb, pressB), "create PRESS-B")

	testutil.MustNoErr(t, nodes.SetMaintainSupports(sdb, grpID,
		[]int64{pressA.ID, pressB.ID}), "SetMaintainSupports both")
	got, err := nodes.ListMaintainSupports(sdb, grpID)
	testutil.MustNoErr(t, err, "ListMaintainSupports")
	if len(got) != 2 || got[0].ProcessNodeName != "PRESS-A" || got[1].ProcessNodeName != "PRESS-B" {
		t.Fatalf("supports = %+v, want PRESS-A and PRESS-B by name", got)
	}

	// Narrowing REMOVES the dropped one.
	testutil.MustNoErr(t, nodes.SetMaintainSupports(sdb, grpID, []int64{pressB.ID}), "narrow to B")
	got, err = nodes.ListMaintainSupports(sdb, grpID)
	testutil.MustNoErr(t, err, "ListMaintainSupports after narrow")
	if len(got) != 1 || got[0].ProcessNodeName != "PRESS-B" {
		t.Errorf("supports after narrow = %+v, want only PRESS-B", got)
	}

	// The empty set is legal — a group mid-configuration supports nobody.
	testutil.MustNoErr(t, nodes.SetMaintainSupports(sdb, grpID, nil), "clear supports")
	got, err = nodes.ListMaintainSupports(sdb, grpID)
	testutil.MustNoErr(t, err, "ListMaintainSupports after clear")
	if len(got) != 0 {
		t.Errorf("supports after clear = %+v, want none", got)
	}
}

// TestMaintainLayerWants_OneTransactionWithTheLevel: a level's save and its
// layer wants commit together or not at all, and the level's remove takes its
// layer wants with it (no foreign key does: v161 declared them against nodes
// and bin_types only).
func TestMaintainLayerWants_OneTransactionWithTheLevel(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	grpID, err := nodes.CreateGroup(sdb, "GRP-LAYERS")
	testutil.MustNoErr(t, err, "CreateGroup")
	testdb.MarkGroup(t, db, grpID)
	tote := mkBinType(t, sdb, "LW-TOTE")

	testutil.MustNoErr(t, nodes.SetMaintainLevelAndLayerWants(sdb,
		nodes.MaintainLevel{GroupNodeID: grpID, BinTypeID: tote, Want: 3},
		[]nodes.MaintainLayerWant{{Layers: 5, Want: 1}, {Layers: 3, Want: 2}}), "save with heights")

	// A height the table refuses (layers >= 0, v164: 0 is the tops & bottoms
	// row) fails the whole save: the level keeps its 3 and the heights stay as
	// they were.
	if err := nodes.SetMaintainLevelAndLayerWants(sdb,
		nodes.MaintainLevel{GroupNodeID: grpID, BinTypeID: tote, Want: 9},
		[]nodes.MaintainLayerWant{{Layers: 2, Want: 1}, {Layers: -1, Want: 1}}); err == nil {
		t.Fatal("a negative height saved; the CHECK should refuse it")
	}
	levels, err := nodes.ListMaintainLevels(sdb, grpID)
	testutil.MustNoErr(t, err, "ListMaintainLevels")
	if len(levels) != 1 || levels[0].Want != 3 {
		t.Errorf("level after a refused save = %+v, want 3: the level and its heights commit together", levels)
	}
	wants, err := nodes.ListMaintainLayerWants(sdb, grpID)
	testutil.MustNoErr(t, err, "ListMaintainLayerWants")
	if len(wants) != 2 || wants[0].Layers != 3 || wants[0].Want != 2 || wants[1].Layers != 5 ||
		wants[0].BinTypeCode != "LW-TOTE" || wants[0].TotesPerLayer != nil {
		t.Errorf("layer wants after a refused save = %+v, want 3 high x2 and 5 high x1, no totes per layer", wants)
	}

	testutil.MustNoErr(t, nodes.RemoveMaintainLevel(sdb, grpID, tote), "remove level")
	wants, err = nodes.ListMaintainLayerWants(sdb, grpID)
	testutil.MustNoErr(t, err, "ListMaintainLayerWants after remove")
	if len(wants) != 0 {
		t.Errorf("layer wants after the level's remove = %+v, want none", wants)
	}
}
