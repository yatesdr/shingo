//go:build docker

package service

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/nodes"
)

// two_stage_pair_docker_test.go — the pair as one unloader: its delete, the
// group Core keeps for a direct pair with several stage-2 windows, and the two
// belts that keep a bare marker out of the carrier-mix and level config.

// TestSetQuota_RefusesBare: a loader's mix names carriers it wants fetched as
// empties, and no finder hands out a bare cart, so a bare line is a mix that can
// never be met. Refused at the save.
func TestSetQuota_RefusesBare(t *testing.T) {
	t.Parallel()
	f := newTwoStageFixture(t)
	marker, err := f.db.EnsureBareMarker(f.carrier.ID)
	testutil.MustNoErr(t, err, "marker")
	id, err := f.loader.CreateLoader(LoaderCreate{Name: "TS-QUOTA", Role: "produce"})
	testutil.MustNoErr(t, err, "create loader")

	if err := f.loader.SetQuota(id, marker, 2); err == nil {
		t.Fatal("SetQuota accepted a bare marker: the mix would want a carrier no empty finder ever hands out")
	}
	quotas, err := f.loader.Quotas(id)
	testutil.MustNoErr(t, err, "quotas")
	if len(quotas) != 0 {
		t.Errorf("quotas = %v, want the refused line not stored", quotas)
	}
}

// TestSetMaintainLevel_RefusesBare: a maintained level keeps N empties of a
// type in a group; a bare cart is nobody's empty, so the keeper would chase a
// level it can never count.
func TestSetMaintainLevel_RefusesBare(t *testing.T) {
	t.Parallel()
	f := newTwoStageFixture(t)
	marker, err := f.db.EnsureBareMarker(f.carrier.ID)
	testutil.MustNoErr(t, err, "marker")
	grpID, _ := mgGroup(t, f.db, "TS-MNT-GRP", 2)

	chk, err := NewNodeService(f.db).SetMaintainLevel(grpID, marker, 1)
	if err == nil && chk.Err() == nil {
		t.Fatal("SetMaintainLevel accepted a bare marker: the keeper would chase a level no finder counts")
	}
	levels, err := f.db.ListMaintainLevels(grpID)
	testutil.MustNoErr(t, err, "levels")
	if len(levels) != 0 {
		t.Errorf("levels = %v, want the refused level not stored", levels)
	}
}

// TestLoaderDelete_Stage2ArchivesPair: the API takes any loader id, and the
// plant sees one unloader, so deleting stage 2 deletes the pair — a stage 1
// left behind would send carts to nothing.
func TestLoaderDelete_Stage2ArchivesPair(t *testing.T) {
	t.Parallel()
	f := newTwoStageFixture(t)
	s1, s2 := f.create(t, "TS-DEL2")
	testutil.MustNoErr(t, f.loader.Delete(s2), "delete stage 2")
	all, err := f.db.ListLoaders()
	testutil.MustNoErr(t, err, "list")
	for _, l := range all {
		if l.ID == s1 || l.ID == s2 {
			t.Errorf("loader %d (%s) still listed after deleting stage 2 of the pair", l.ID, l.Name)
		}
	}
}

// TestPairDelete_ReparentsStage2NodesAndDropsGroup: a direct pair with two
// stage-2 windows sends to a group Core made for it. Deleting the pair gives the
// windows back to the grid and drops the group, which would otherwise be debris
// holding the windows and blocking a re-create.
func TestPairDelete_ReparentsStage2NodesAndDropsGroup(t *testing.T) {
	t.Parallel()
	f := newTwoStageFixture(t)
	s1, s2 := f.create(t, "TS-GRPDEL")
	second := &nodes.Node{Name: "TS-S2-B", Enabled: true}
	testutil.MustNoErr(t, f.db.CreateNode(second), "second window")
	testutil.MustNoErr(t, f.loader.SetHome(s2, f.stage2.ID, "", "", 0), "window A")
	testutil.MustNoErr(t, f.loader.SetHome(s2, second.ID, "", "", 0), "window B")

	one, err := f.db.GetLoader(s1)
	testutil.MustNoErr(t, err, "stage 1")
	grp, err := f.db.GetNodeByDotName(one.OutboundDest)
	if err != nil || grp == nil || !grp.IsSynthetic {
		t.Fatalf("stage 1 sends to %q, want a group Core made holding both stage-2 windows", one.OutboundDest)
	}
	for _, id := range []int64{f.stage2.ID, second.ID} {
		n, err := f.db.GetNode(id)
		testutil.MustNoErr(t, err, "window")
		if n.ParentID == nil || *n.ParentID != grp.ID {
			t.Fatalf("window %s parent = %v, want the pair's group %d", n.Name, n.ParentID, grp.ID)
		}
	}

	testutil.MustNoErr(t, f.loader.Delete(s1), "delete the pair")
	for _, id := range []int64{f.stage2.ID, second.ID} {
		n, err := f.db.GetNode(id)
		testutil.MustNoErr(t, err, "window after")
		if n.ParentID != nil {
			t.Errorf("window %s still parented to %d after the pair was deleted", n.Name, *n.ParentID)
		}
	}
	if g, err := f.db.GetNode(grp.ID); err == nil && g != nil {
		t.Errorf("the pair's group %s (%d) survived the pair's delete", g.Name, g.ID)
	}
}

// TestPairRename_KeepsItsGroup: a direct pair with two stage-2 windows sends to
// a group Core named after the pair. Renaming stage 1 used to derive a new group
// name, create that group empty, and then refuse because the windows were still
// in the old one — after the rename and a blank stage-1 destination had already
// been written, so the next CLEAR had nowhere to send the cart. The rename now
// carries the group with it.
func TestPairRename_KeepsItsGroup(t *testing.T) {
	t.Parallel()
	f := newTwoStageFixture(t)
	s1, s2 := f.create(t, "TS-REN")
	second := &nodes.Node{Name: "TS-REN-S2-B", Enabled: true}
	testutil.MustNoErr(t, f.db.CreateNode(second), "second window")
	testutil.MustNoErr(t, f.loader.SetHome(s2, f.stage2.ID, "", "", 0), "window A")
	testutil.MustNoErr(t, f.loader.SetHome(s2, second.ID, "", "", 0), "window B")

	one, err := f.db.GetLoader(s1)
	testutil.MustNoErr(t, err, "stage 1")
	before, err := f.db.GetNodeByDotName(one.OutboundDest)
	testutil.MustNoErr(t, err, "the pair's group")

	testutil.MustNoErr(t, f.loader.Update(LoaderUpdate{
		ID: s1, Name: "TS-RENAMED · stage 1", Layout: one.Layout, Replenishment: one.Replenishment,
		OutboundDest: "", InboundSource: one.InboundSource, FedDirectly: one.FedDirectly,
		AcceptPartials: one.AcceptPartials, AutoPush: one.AutoPush,
	}), "rename stage 1")

	after, err := f.db.GetLoader(s1)
	testutil.MustNoErr(t, err, "stage 1 after")
	if after.Name != "TS-RENAMED · stage 1" {
		t.Fatalf("name = %q, want the rename stored", after.Name)
	}
	grp, err := f.db.GetNodeByDotName(after.OutboundDest)
	if err != nil || grp == nil || grp.ID != before.ID {
		t.Fatalf("stage 1 sends to %q, want the pair's same group %d (renamed), not a new one", after.OutboundDest, before.ID)
	}
	if grp.Name != "TS-RENAMED · stage 2" {
		t.Errorf("group name = %q, want it to follow the pair's name", grp.Name)
	}
	for _, id := range []int64{f.stage2.ID, second.ID} {
		n, err := f.db.GetNode(id)
		testutil.MustNoErr(t, err, "window")
		if n.ParentID == nil || *n.ParentID != grp.ID {
			t.Errorf("window %s parent = %v, want the pair's group %d", n.Name, n.ParentID, grp.ID)
		}
	}
}
