//go:build docker

package service

import (
	"reflect"
	"testing"

	"shingo/protocol/testutil"
)

// Every level save that writes calls the level hook once, with the group: the
// level alone, the level with its layer wants, and the removal. A refused save
// writes nothing and calls nothing.
func TestKanbanLevelSaves_CallTheLevelHook(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)
	var got []int64
	svc.SetLevelChangedFunc(func(groupNodeID int64) { got = append(got, groupNodeID) })
	grpID, _ := mgGroup(t, db, "GRP-HOOK", 3)
	bt := mgBinType(t, db, "HOOK-TOTE")

	chk, err := svc.SetKanbanLevel(grpID, bt, -1)
	testutil.MustNoErr(t, err, "refused save")
	if chk.Err() == nil || len(got) != 0 {
		t.Fatalf("a negative level: refused %v, hook calls %v; want refused and none", chk.Refusals, got)
	}
	for _, save := range []func() (KanbanCheck, error){
		func() (KanbanCheck, error) { return svc.SetKanbanLevel(grpID, bt, 2) },
		func() (KanbanCheck, error) { return svc.SetKanbanLevelAndLayerWants(grpID, bt, 2, nil) },
		func() (KanbanCheck, error) { return svc.RemoveKanbanLevel(grpID, bt) },
	} {
		chk, err := save()
		testutil.MustNoErr(t, err, "save")
		if chk.Err() != nil {
			t.Fatalf("save refused: %v", chk.Refusals)
		}
	}
	if want := []int64{grpID, grpID, grpID}; !reflect.DeepEqual(got, want) {
		t.Errorf("hook calls = %v, want %v", got, want)
	}
}

// TestKanbanSaves_RingTheLineOnlyWhenTheLineMoves: a level, the cover and the supports
// are in what an Edge is shown; the sources, an overflow and the rest of the settings
// are not.
func TestKanbanSaves_RingTheLineOnlyWhenTheLineMoves(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)
	calls := 0
	svc.SetLevelChangedFunc(func(int64) { calls++ })
	grpID, _ := mgGroup(t, db, "GRP-RING", 3)
	bt := mgBinType(t, db, "RING-TOTE")
	srcID, _ := mgGroup(t, db, "GRP-RING-SRC", 1)
	press := mgSupportNode(t, db, "RING-SVC-PRESS")
	zero, cover, station, strict, overflow := int64(0), 2, "EDGE-1", true, "GRP-RING-SRC"
	want := 1

	for _, c := range []struct {
		what  string
		save  KanbanSave
		rings bool
	}{
		{"a level", KanbanSave{Levels: []KanbanLevelSave{{BinTypeID: bt, Want: &want}}}, true},
		{"the cover", KanbanSave{Cover: &cover}, true},
		{"the station", KanbanSave{Station: &station}, false},
		{"the supports", KanbanSave{Supports: &[]int64{press}}, true},
		{"the sources", KanbanSave{Sources: &[]int64{srcID}}, false},
		{"the default overflow", KanbanSave{Overflow: &overflow}, false},
		{"a type's overflow", KanbanSave{Levels: []KanbanLevelSave{{BinTypeID: bt, Overflow: &srcID}}}, false},
		{"a type's overflow cleared", KanbanSave{Levels: []KanbanLevelSave{{BinTypeID: bt, Overflow: &zero}}}, false},
		{"strict", KanbanSave{Strict: &strict}, false},
	} {
		calls = 0
		c.save.GroupNodeID = grpID
		chk, err := svc.SaveKanban(c.save)
		testutil.MustNoErr(t, err, c.what)
		if chk.Err() != nil {
			t.Fatalf("%s refused: %v", c.what, chk.Refusals)
		}
		if (calls == 1) != c.rings || calls > 1 {
			t.Errorf("%s: %d rings, want rings=%v", c.what, calls, c.rings)
		}
	}
}
