//go:build docker

package service

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// loader_two_stage_docker_test.go — a two-stage unloader set up as one: both
// stages and the link, with where stage 1 sends a cart derived from stage 2.

type twoStageFixture struct {
	db      *store.DB
	loader  *LoaderService
	carrier *bins.BinType
	stage2  *nodes.Node
	out     *nodes.Node
}

func newTwoStageFixture(t *testing.T) twoStageFixture {
	t.Helper()
	db := testDB(t)
	f := twoStageFixture{
		db:      db,
		loader:  NewLoaderService(db, nil),
		carrier: &bins.BinType{Code: "TS-CART", RequiredRobotGroup: "CART-BOTS"},
		stage2:  &nodes.Node{Name: "TS-S2-A", Enabled: true},
		out:     &nodes.Node{Name: "TS-EMPTIES", Enabled: true},
	}
	testutil.MustNoErr(t, db.CreateBinType(f.carrier), "create carrier type")
	testutil.MustNoErr(t, db.CreateNode(f.stage2), "create stage-2 node")
	testutil.MustNoErr(t, db.CreateNode(f.out), "create empties node")
	return f
}

func (f twoStageFixture) create(t *testing.T, name string) (int64, int64) {
	t.Helper()
	s1, s2, err := f.loader.CreateTwoStage(TwoStageCreate{Name: name, AcceptPartials: true, AutoPush: true})
	testutil.MustNoErr(t, err, "create two-stage")
	return s1, s2
}

func TestCreateTwoStage_LinksStagesWithNoPlacement(t *testing.T) {
	t.Parallel()
	f := newTwoStageFixture(t)
	s1, s2 := f.create(t, "TS-UNLOADER")

	one, err := f.db.GetLoader(s1)
	testutil.MustNoErr(t, err, "read stage 1")
	if one.Role != loaders.RoleConsume || one.SecondStageLoaderID == nil || *one.SecondStageLoaderID != s2 {
		t.Errorf("stage 1 = role %s link %v, want an unloader naming stage 2 %d", one.Role, one.SecondStageLoaderID, s2)
	}
	if one.OutboundDest != "" || !one.AcceptPartials || !one.AutoPush {
		t.Errorf("stage 1 = out %q partials %v push %v, want nowhere yet, true, true",
			one.OutboundDest, one.AcceptPartials, one.AutoPush)
	}
	two, err := f.db.GetLoader(s2)
	testutil.MustNoErr(t, err, "read stage 2")
	if two.Role != loaders.RoleConsume || two.SecondStageLoaderID != nil || two.OutboundDest != "" || two.InboundSource != "" {
		t.Errorf("stage 2 = %+v, want a plain unloader with nothing placed", two)
	}
	if _, _, err := f.loader.CreateTwoStage(TwoStageCreate{}); err == nil {
		t.Error("a pair with no name was created")
	}
}

// Direct mode: stage 1 sends to stage 2's one window; a window standing in
// another group is refused, naming the group.
func TestTwoStage_DirectModeDerivesStage1Destination(t *testing.T) {
	t.Parallel()
	f := newTwoStageFixture(t)
	s1, s2 := f.create(t, "TS-DIRECT")
	testutil.MustNoErr(t, f.loader.SetHome(s2, f.stage2.ID, "", "", 0), "stage-2 window")
	one, err := f.db.GetLoader(s1)
	testutil.MustNoErr(t, err, "read stage 1")
	if one.OutboundDest != f.stage2.Name {
		t.Errorf("stage 1 sends to %q, want the one stage-2 window %s", one.OutboundDest, f.stage2.Name)
	}

	grpID, err := f.db.CreateNodeGroup("TS-SOMEONE-ELSES")
	testutil.MustNoErr(t, err, "other group")
	taken := &nodes.Node{Name: "TS-TAKEN", Enabled: true, ParentID: &grpID}
	testutil.MustNoErr(t, f.db.CreateNode(taken), "node in another group")
	err = f.loader.SetHome(s2, taken.ID, "", "", 0)
	if !errors.Is(err, ErrWindowInAnotherGroup) || !strings.Contains(err.Error(), "TS-SOMEONE-ELSES") {
		t.Errorf("window in another group: err = %v, want ErrWindowInAnotherGroup naming TS-SOMEONE-ELSES", err)
	}

	// Two windows then one: the group Core made comes and goes with them.
	second := &nodes.Node{Name: "TS-S2-B", Enabled: true}
	testutil.MustNoErr(t, f.db.CreateNode(second), "second window")
	testutil.MustNoErr(t, f.loader.SetHome(s2, second.ID, "", "", 0), "second stage-2 window")
	one, err = f.db.GetLoader(s1)
	testutil.MustNoErr(t, err, "read stage 1")
	grp, err := f.db.GetNodeByDotName(one.OutboundDest)
	if err != nil || grp == nil || !grp.IsSynthetic {
		t.Fatalf("stage 1 sends to %q with two windows, want the group Core made", one.OutboundDest)
	}
	testutil.MustNoErr(t, f.loader.RemoveHome(s2, second.ID), "remove a window")
	one, err = f.db.GetLoader(s1)
	testutil.MustNoErr(t, err, "read stage 1")
	if one.OutboundDest != f.stage2.Name {
		t.Errorf("back to one window, stage 1 sends to %q, want %s", one.OutboundDest, f.stage2.Name)
	}
	for _, id := range []int64{f.stage2.ID, second.ID} {
		if n, err := f.db.GetNode(id); err != nil || n.ParentID != nil {
			t.Errorf("node %d still in a group after the pair went back to one window (err %v)", id, err)
		}
	}
	if g, err := f.db.GetNode(grp.ID); err == nil && g != nil {
		t.Errorf("the pair's group %s survived going back to one window", g.Name)
	}
}

// Pull mode: stage 2 names a wait group, and stage 1 sends there — whatever a
// stage-1 update says.
func TestTwoStage_PullModeSendsStage1ToTheWaitGroup(t *testing.T) {
	t.Parallel()
	f := newTwoStageFixture(t)
	s1, s2 := f.create(t, "TS-PULL")
	grpID, err := f.db.CreateNodeGroup("TS-WAIT")
	testutil.MustNoErr(t, err, "wait group")
	testutil.MustNoErr(t, f.loader.SetHome(s2, f.stage2.ID, "", "", 0), "stage-2 window")
	two, err := f.db.GetLoader(s2)
	testutil.MustNoErr(t, err, "read stage 2")
	testutil.MustNoErr(t, f.loader.Update(LoaderUpdate{ID: s2, Name: two.Name, InboundSource: "TS-WAIT"}), "pull from the wait group")
	one, err := f.db.GetLoader(s1)
	testutil.MustNoErr(t, err, "read stage 1")
	if one.OutboundDest != "TS-WAIT" {
		t.Errorf("stage 1 sends to %q, want the wait group TS-WAIT", one.OutboundDest)
	}
	testutil.MustNoErr(t, f.loader.Update(LoaderUpdate{ID: s1, Name: one.Name, OutboundDest: f.out.Name,
		AcceptPartials: one.AcceptPartials, AutoPush: one.AutoPush}), "stage-1 update naming something else")
	one, err = f.db.GetLoader(s1)
	testutil.MustNoErr(t, err, "read stage 1")
	if one.OutboundDest != "TS-WAIT" {
		t.Errorf("stage 1 sends to %q after a stage-1 update, want it derived: TS-WAIT", one.OutboundDest)
	}
	if n, err := f.db.GetNode(f.stage2.ID); err != nil || (n.ParentID != nil && *n.ParentID == grpID) {
		t.Errorf("pull mode parented the stage-2 window into the wait group (err %v)", err)
	}
}

func TestTwoStage_DeletingStage1TakesStage2(t *testing.T) {
	t.Parallel()
	f := newTwoStageFixture(t)
	s1, s2 := f.create(t, "TS-DELETE")
	testutil.MustNoErr(t, f.loader.Delete(s1), "delete stage 1")
	all, err := f.db.ListLoaders()
	testutil.MustNoErr(t, err, "list loaders")
	for _, l := range all {
		if l.ID == s1 || l.ID == s2 {
			t.Errorf("loader %d (%s) still listed after deleting the two-stage unloader", l.ID, l.Name)
		}
	}
}

func TestCreateLoader_TakesUnloaderSettingsAtCreate(t *testing.T) {
	t.Parallel()
	f := newTwoStageFixture(t)
	id, err := f.loader.CreateLoader(LoaderCreate{Name: "TS-ONE", Role: loaders.RoleConsume, AcceptPartials: true, AutoPush: true})
	testutil.MustNoErr(t, err, "create unloader with settings")
	got, err := f.db.GetLoader(id)
	testutil.MustNoErr(t, err, "read")
	if !got.AcceptPartials || !got.AutoPush {
		t.Errorf("created with partials %v push %v, want both", got.AcceptPartials, got.AutoPush)
	}
	if _, err := f.loader.CreateLoader(LoaderCreate{Name: "TS-LOAD", Role: loaders.RoleProduce, AutoPush: true}); !errors.Is(err, ErrAutoPushProduce) {
		t.Errorf("auto push on a loader: err = %v, want ErrAutoPushProduce", err)
	}
}
