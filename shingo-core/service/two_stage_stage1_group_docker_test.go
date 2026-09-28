//go:build docker

package service

import (
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/nodes"
)

// two_stage_stage1_group_docker_test.go — stage 1's windows stand in a group
// Core makes, so a press or cell feeding stage 1 has one name to pick as its
// outbound destination.

// stage1Parent returns the group node id stands in, failing when it stands in
// none.
func stage1Parent(t *testing.T, f twoStageFixture, id int64) *nodes.Node {
	t.Helper()
	n, err := f.db.GetNode(id)
	testutil.MustNoErr(t, err, "window")
	if n.ParentID == nil {
		t.Fatalf("window %s stands in no group, want stage 1's", n.Name)
	}
	g, err := f.db.GetNode(*n.ParentID)
	testutil.MustNoErr(t, err, "window's group")
	return g
}

func TestTwoStage_Stage1WindowsStandInAGroup(t *testing.T) {
	t.Parallel()
	f := newTwoStageFixture(t)
	s1, _ := f.create(t, "TS-FEED")
	a := &nodes.Node{Name: "TS-S1-A", Enabled: true}
	b := &nodes.Node{Name: "TS-S1-B", Enabled: true}
	testutil.MustNoErr(t, f.db.CreateNode(a), "window A")
	testutil.MustNoErr(t, f.db.CreateNode(b), "window B")

	// Made at the first window, so the name a press claim names never changes
	// when a second window is added.
	testutil.MustNoErr(t, f.loader.SetHome(s1, a.ID, "", "", 0), "stage-1 window A")
	grp := stage1Parent(t, f, a.ID)
	if !grp.IsSynthetic || grp.Name != "TS-FEED · stage 1" {
		t.Fatalf("stage-1 window stands in %q (synthetic %v), want the group Core made, TS-FEED · stage 1", grp.Name, grp.IsSynthetic)
	}
	testutil.MustNoErr(t, f.loader.SetHome(s1, b.ID, "", "", 0), "stage-1 window B")
	if g := stage1Parent(t, f, b.ID); g.ID != grp.ID {
		t.Errorf("second window stands in %s, want the same group %s", g.Name, grp.Name)
	}

	// A window standing in someone else's group is refused, not taken.
	otherID, err := f.db.CreateNodeGroup("TS-PLANT-GROUP")
	testutil.MustNoErr(t, err, "other group")
	taken := &nodes.Node{Name: "TS-S1-TAKEN", Enabled: true, ParentID: &otherID}
	testutil.MustNoErr(t, f.db.CreateNode(taken), "node in another group")
	if err := f.loader.SetHome(s1, taken.ID, "", "", 0); !errors.Is(err, ErrWindowInAnotherGroup) {
		t.Errorf("stage-1 window in another group: err = %v, want ErrWindowInAnotherGroup", err)
	}

	// Pulling from a group instead of being fed directly keeps it: Core's own
	// pulls name the window.
	one, err := f.db.GetLoader(s1)
	testutil.MustNoErr(t, err, "stage 1")
	_, err = f.db.CreateNodeGroup("TS-FULLS")
	testutil.MustNoErr(t, err, "fulls group")
	testutil.MustNoErr(t, f.loader.Update(LoaderUpdate{ID: s1, Name: one.Name, Layout: one.Layout,
		Replenishment: one.Replenishment, InboundSource: "TS-FULLS",
		AcceptPartials: one.AcceptPartials, AutoPush: one.AutoPush}), "pull from TS-FULLS")
	if g := stage1Parent(t, f, a.ID); g.ID != grp.ID {
		t.Errorf("after setting an inbound source window A stands in %s, want %s", g.Name, grp.Name)
	}

	// A rename carries the group and its name.
	one, err = f.db.GetLoader(s1)
	testutil.MustNoErr(t, err, "stage 1")
	testutil.MustNoErr(t, f.loader.Update(LoaderUpdate{ID: s1, Name: "TS-FED · stage 1", Layout: one.Layout,
		Replenishment: one.Replenishment, InboundSource: one.InboundSource,
		AcceptPartials: one.AcceptPartials, AutoPush: one.AutoPush}), "rename")
	if g := stage1Parent(t, f, a.ID); g.ID != grp.ID || g.Name != "TS-FED · stage 1" {
		t.Errorf("after the rename window A stands in %s (%d), want group %d renamed TS-FED · stage 1", g.Name, g.ID, grp.ID)
	}

	// The last window out drops the group and gives the windows back.
	testutil.MustNoErr(t, f.loader.RemoveHome(s1, a.ID), "remove A")
	if n, err := f.db.GetNode(a.ID); err != nil || n.ParentID != nil {
		t.Errorf("removed window A still in a group (err %v)", err)
	}
	testutil.MustNoErr(t, f.loader.RemoveHome(s1, b.ID), "remove B")
	if n, err := f.db.GetNode(b.ID); err != nil || n.ParentID != nil {
		t.Errorf("removed window B still in a group (err %v)", err)
	}
	if g, err := f.db.GetNode(grp.ID); err == nil && g != nil {
		t.Errorf("stage 1's group %s survived its last window", g.Name)
	}
}

func TestPairDelete_DropsStage1Group(t *testing.T) {
	t.Parallel()
	f := newTwoStageFixture(t)
	s1, _ := f.create(t, "TS-S1DEL")
	a := &nodes.Node{Name: "TS-S1DEL-A", Enabled: true}
	testutil.MustNoErr(t, f.db.CreateNode(a), "window")
	testutil.MustNoErr(t, f.loader.SetHome(s1, a.ID, "", "", 0), "stage-1 window")
	grp := stage1Parent(t, f, a.ID)

	testutil.MustNoErr(t, f.loader.Delete(s1), "delete the pair")
	if n, err := f.db.GetNode(a.ID); err != nil || n.ParentID != nil {
		t.Errorf("window still in a group after the pair was deleted (err %v)", err)
	}
	if g, err := f.db.GetNode(grp.ID); err == nil && g != nil {
		t.Errorf("stage 1's group %s survived the pair's delete", g.Name)
	}
}

// SyncPairs gives a pair set up before stage 1 had a group its group.
func TestSyncPairs_GroupsAnExistingStage1(t *testing.T) {
	t.Parallel()
	f := newTwoStageFixture(t)
	s1, _ := f.create(t, "TS-OLD")
	a := &nodes.Node{Name: "TS-OLD-A", Enabled: true}
	testutil.MustNoErr(t, f.db.CreateNode(a), "window")
	testutil.MustNoErr(t, f.loader.SetHome(s1, a.ID, "", "", 0), "stage-1 window")
	// Undo what SetHome did, to stand for a pair from before this change.
	grp := stage1Parent(t, f, a.ID)
	testutil.MustNoErr(t, f.db.DeleteNodeGroup(grp.ID), "drop the group")

	f.loader.SyncPairs()
	if g := stage1Parent(t, f, a.ID); g.Name != "TS-OLD · stage 1" {
		t.Errorf("after SyncPairs window stands in %s, want TS-OLD · stage 1", g.Name)
	}
}
