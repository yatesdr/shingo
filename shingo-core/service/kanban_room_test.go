//go:build docker

package service

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
)

// TestKanbanFamilies_TheRowCountsBesideTheLevel: a group keeping three of a type and a tops &
// bottoms row of two stands five carts, whatever heights of 1 or more it also wants (those are
// the level said by height). Past its slots the save is warned and never refused, and the card's
// row says what cannot be covered.
func TestKanbanFamilies_TheRowCountsBesideTheLevel(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)
	grpID, kids := mgGroup(t, db, "TB-GRP", 4)
	var bt int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, totes_per_layer) VALUES ('TB-TOTE', 8) RETURNING id`).Scan(&bt), "type")
	want := 3
	wants := []store.KanbanLayerWant{{Layers: 0, Want: 2}, {Layers: 2, Want: 3}}
	chk, err := svc.SaveKanban(KanbanSave{GroupNodeID: grpID, Levels: []KanbanLevelSave{{BinTypeID: bt, Want: &want, LayerWants: &wants}}})
	testutil.MustNoErr(t, err, "the save")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}
	if said := strings.Join(chk.Warnings, "; "); !strings.Contains(said, "room for 4") {
		t.Errorf("warnings = %v, want room for 4 of the 5", chk.Warnings)
	}
	rows, err := svc.ListKanbanFamilies(grpID)
	testutil.MustNoErr(t, err, "the rows")
	if len(rows) != 1 || rows[0].Minimum != 5 || rows[0].Pool != 5 || !rows[0].PoolDeclared || rows[0].Room != 4 || rows[0].CannotCover != 1 {
		t.Errorf("rows = %+v, want minimum and pool 5, room 4, 1 that cannot be covered", rows)
	}

	// A disabled slot is no room.
	kids[0].Enabled = false
	testutil.MustNoErr(t, db.UpdateNode(kids[0]), "disable")
	rows, err = svc.ListKanbanFamilies(grpID)
	testutil.MustNoErr(t, err, "the rows")
	if rows[0].Room != 3 {
		t.Errorf("room = %d with a slot disabled, want 3", rows[0].Room)
	}
}
