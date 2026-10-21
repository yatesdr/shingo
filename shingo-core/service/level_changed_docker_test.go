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
func TestMaintainLevelSaves_CallTheLevelHook(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)
	var got []int64
	svc.SetLevelChangedFunc(func(groupNodeID int64) { got = append(got, groupNodeID) })
	grpID, _ := mgGroup(t, db, "GRP-HOOK", 3)
	bt := mgBinType(t, db, "HOOK-TOTE")

	chk, err := svc.SetMaintainLevel(grpID, bt, -1)
	testutil.MustNoErr(t, err, "refused save")
	if chk.Err() == nil || len(got) != 0 {
		t.Fatalf("a negative level: refused %v, hook calls %v; want refused and none", chk.Refusals, got)
	}
	for _, save := range []func() (MaintainedGroupCheck, error){
		func() (MaintainedGroupCheck, error) { return svc.SetMaintainLevel(grpID, bt, 2) },
		func() (MaintainedGroupCheck, error) { return svc.SetMaintainLevelAndLayerWants(grpID, bt, 2, nil) },
		func() (MaintainedGroupCheck, error) { return svc.RemoveMaintainLevel(grpID, bt) },
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
