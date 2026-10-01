//go:build docker

package service

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// two_stage_pulled_directly_docker_test.go — "Pulled directly by the process"
// on a two-stage unloader's stage 2: its finished carts stay on its windows,
// the windows stand in a group Core makes at the first one, in both modes, and
// a line naming that group sources its empty there. The option off is pinned
// by TestTwoStage_DirectModeDerivesStage1Destination and
// TestTwoStage_PullModeSendsStage1ToTheWaitGroup, unchanged.

// pulledDirectly turns the option on (or off) on stage 2, keeping the rest of
// its setup as stored. outbound is what the form sent, which the option blanks.
func (f twoStageFixture) pulledDirectly(t *testing.T, s2 int64, on bool, outbound string) error {
	t.Helper()
	two, err := f.db.GetLoader(s2)
	testutil.MustNoErr(t, err, "read stage 2")
	return f.loader.Update(LoaderUpdate{ID: s2, Name: two.Name, InboundSource: two.InboundSource,
		OutboundDest: outbound, AcceptPartials: two.AcceptPartials, AutoPush: two.AutoPush, PulledDirectly: on})
}

// stage2Group is the group stage 2's first window stands in, or nil.
func (f twoStageFixture) stage2Group(t *testing.T) *nodes.Node {
	t.Helper()
	n, err := f.db.GetNode(f.stage2.ID)
	testutil.MustNoErr(t, err, "read the stage-2 window")
	if n.ParentID == nil {
		return nil
	}
	g, err := f.db.GetNode(*n.ParentID)
	testutil.MustNoErr(t, err, "read its group")
	return g
}

// setUpMode makes a pair with one stage-2 window, in pull mode (a wait group)
// or direct mode, and turns the option on.
func (f twoStageFixture) setUpMode(t *testing.T, name string, pull bool) (int64, int64) {
	t.Helper()
	s1, s2 := f.create(t, name)
	testutil.MustNoErr(t, f.loader.SetHome(s2, f.stage2.ID, "", "", 0), "stage-2 window")
	if pull {
		_, err := f.db.CreateNodeGroup(name + "-WAIT")
		testutil.MustNoErr(t, err, "wait group")
		two, err := f.db.GetLoader(s2)
		testutil.MustNoErr(t, err, "read stage 2")
		testutil.MustNoErr(t, f.loader.Update(LoaderUpdate{ID: s2, Name: two.Name, InboundSource: name + "-WAIT",
			AcceptPartials: two.AcceptPartials, AutoPush: two.AutoPush}), "pull mode")
	}
	testutil.MustNoErr(t, f.pulledDirectly(t, s2, true, f.out.Name), "pulled directly on")
	return s1, s2
}

func TestPulledDirectly_GroupsStage2AtItsFirstWindowInBothModes(t *testing.T) {
	t.Parallel()
	for _, pull := range []bool{false, true} {
		f := newTwoStageFixture(t)
		mode := map[bool]string{false: "direct", true: "pull"}[pull]
		s1, s2 := f.setUpMode(t, "TS-PD-"+strings.ToUpper(mode), pull)

		one, err := f.db.GetLoader(s1)
		testutil.MustNoErr(t, err, "read stage 1")
		g := f.stage2Group(t)
		if g == nil || !g.IsSynthetic || g.Name != pairGroupName(one) {
			t.Fatalf("%s mode: stage 2's one window stands in %v, want the group Core made for it", mode, g)
		}
		two, err := f.db.GetLoader(s2)
		testutil.MustNoErr(t, err, "read stage 2")
		if two.OutboundDest != "" || !two.PulledDirectly {
			t.Errorf("%s mode: stage 2 stores outbound %q pulled=%v, want blank and on: its carts are not sent anywhere",
				mode, two.OutboundDest, two.PulledDirectly)
		}
		want := g.Name // direct: stage 1 sends into the group, even with one window
		if pull {
			want = "TS-PD-PULL-WAIT" // pull: still the wait group; the stage-2 pull moves carts in
		}
		if one.OutboundDest != want {
			t.Errorf("%s mode: stage 1 sends to %q, want %q", mode, one.OutboundDest, want)
		}
	}
}

func TestPulledDirectly_RefusesAWindowInAnotherGroup(t *testing.T) {
	t.Parallel()
	for _, pull := range []bool{false, true} {
		f := newTwoStageFixture(t)
		mode := map[bool]string{false: "DIRECT", true: "PULL"}[pull]
		_, s2 := f.setUpMode(t, "TS-PDG-"+mode, pull)
		grpID, err := f.db.CreateNodeGroup("TS-OTHERS-" + mode)
		testutil.MustNoErr(t, err, "other group")
		taken := &nodes.Node{Name: "TS-TAKEN-" + mode, Enabled: true, ParentID: &grpID}
		testutil.MustNoErr(t, f.db.CreateNode(taken), "node in another group")
		err = f.loader.SetHome(s2, taken.ID, "", "", 0)
		if !errors.Is(err, ErrWindowInAnotherGroup) || !strings.Contains(err.Error(), "TS-OTHERS-"+mode) {
			t.Errorf("%s mode: a window in another group: err = %v, want ErrWindowInAnotherGroup naming it", mode, err)
		}
	}
}

func TestPulledDirectly_IsStage2Only(t *testing.T) {
	t.Parallel()
	f := newTwoStageFixture(t)
	s1, _ := f.create(t, "TS-PD-S1")
	one, err := f.db.GetLoader(s1)
	testutil.MustNoErr(t, err, "read stage 1")
	err = f.loader.Update(LoaderUpdate{ID: s1, Name: one.Name, AcceptPartials: one.AcceptPartials,
		AutoPush: one.AutoPush, PulledDirectly: true})
	if !errors.Is(err, ErrPulledDirectlyStage2Only) {
		t.Errorf("stage 1 pulled directly: err = %v, want ErrPulledDirectlyStage2Only", err)
	}
	if _, err := f.loader.CreateLoader(LoaderCreate{Name: "TS-PD-LONE", Role: "consume", PulledDirectly: true}); !errors.Is(err, ErrPulledDirectlyStage2Only) {
		t.Errorf("a lone unloader pulled directly: err = %v, want ErrPulledDirectlyStage2Only", err)
	}
}

// The option off again gives the pair back today's shape: one direct window is
// a node stage 1 sends to, and a pull-mode window stands in no group.
func TestPulledDirectly_OffRestoresTodaysShape(t *testing.T) {
	t.Parallel()
	for _, pull := range []bool{false, true} {
		f := newTwoStageFixture(t)
		mode := map[bool]string{false: "DIRECT", true: "PULL"}[pull]
		s1, s2 := f.setUpMode(t, "TS-PDO-"+mode, pull)
		testutil.MustNoErr(t, f.pulledDirectly(t, s2, false, f.out.Name), "off")
		if g := f.stage2Group(t); g != nil {
			t.Errorf("%s mode: off, the window still stands in %s", mode, g.Name)
		}
		one, err := f.db.GetLoader(s1)
		testutil.MustNoErr(t, err, "read stage 1")
		want := f.stage2.Name
		if pull {
			want = "TS-PDO-PULL-WAIT"
		}
		if one.OutboundDest != want {
			t.Errorf("%s mode: off, stage 1 sends to %q, want %q", mode, one.OutboundDest, want)
		}
		two, err := f.db.GetLoader(s2)
		testutil.MustNoErr(t, err, "read stage 2")
		if two.OutboundDest != f.out.Name {
			t.Errorf("%s mode: off, stage 2 sends to %q, want what the form sent: %s", mode, two.OutboundDest, f.out.Name)
		}
	}
}

// The finders: a finished cart on a pulled-directly stage 2's window is an
// empty only to a request naming that stage 2's group, and a bare cart never
// is. The level keeper's count reads the same WHERE, so it agrees.
func TestPulledDirectly_FindersHandTheCartOnlyToItsGroup(t *testing.T) {
	t.Parallel()
	for _, pull := range []bool{false, true} {
		f := newTwoStageFixture(t)
		mode := map[bool]string{false: "DIRECT", true: "PULL"}[pull]
		_, s2 := f.setUpMode(t, "TS-PDF-"+mode, pull)
		g := f.stage2Group(t)
		if g == nil {
			t.Fatalf("%s mode: no stage-2 group to source from", mode)
		}
		testutil.MustNoErr(t, f.db.CreateBin(&bins.Bin{Label: "TS-FINISHED-" + mode, BinTypeID: f.carrier.ID,
			NodeID: &f.stage2.ID, Status: domain.BinStatusAvailable}), "a finished cart on the window")
		none := reservations.DigAsker{}

		b, err := f.db.FindEmptyBinOfTypeInGroup(f.carrier.Code, g.ID, 0, none)
		if err != nil || b == nil || b.Label != "TS-FINISHED-"+mode {
			t.Errorf("%s mode: a line naming %s got %v (err %v), want the finished cart", mode, g.Name, b, err)
		}
		if b, err := f.db.FindEmptyCompatibleBinInGroup("", g.ID, 0, none); err != nil || b == nil {
			t.Errorf("%s mode: the compatible finder naming %s got %v (err %v), want the finished cart", mode, g.Name, b, err)
		}
		if n, err := f.db.CountEmptyBinsOfTypeInGroup(f.carrier.Code, g.ID); err != nil || n != 1 {
			t.Errorf("%s mode: the keeper counts %d in %s (err %v), want 1: count and finder agree", mode, n, g.Name, err)
		}

		// Not naming the group: the plant-wide finder and another group get nothing.
		if b, err := f.db.FindEmptyBinOfType(f.carrier.Code, "", 0, bins.EmptyFence{}, none); err == nil && b != nil {
			t.Errorf("%s mode: a request naming no group took %s off a stage-2 window", mode, b.Label)
		}
		otherID, err := f.db.CreateNodeGroup("TS-ELSEWHERE-" + mode)
		testutil.MustNoErr(t, err, "another group")
		if b, err := f.db.FindEmptyBinOfTypeInGroup(f.carrier.Code, otherID, 0, none); err == nil && b != nil {
			t.Errorf("%s mode: a request naming another group took %s", mode, b.Label)
		}

		// A bare cart on the window is never an empty, even to its own group.
		marker, err := f.db.EnsureBareMarker(f.carrier.ID)
		testutil.MustNoErr(t, err, "marker")
		second := &nodes.Node{Name: "TS-S2-BARE-" + mode, Enabled: true}
		testutil.MustNoErr(t, f.db.CreateNode(second), "second window")
		testutil.MustNoErr(t, f.loader.SetHome(s2, second.ID, "", "", 0), "second window")
		testutil.MustNoErr(t, f.db.CreateBin(&bins.Bin{Label: "TS-BARE-" + mode, BinTypeID: marker,
			NodeID: &second.ID, Status: domain.BinStatusAvailable}), "a bare cart on the window")
		if n, err := f.db.CountEmptyBinsOfTypeInGroup(f.carrier.Code, g.ID); err != nil || n != 1 {
			t.Errorf("%s mode: with a bare cart beside it the keeper counts %d (err %v), want 1", mode, n, err)
		}
		for i := 0; i < 2; i++ {
			b, err := f.db.FindEmptyCompatibleBinInGroup("", g.ID, 0, none)
			if err == nil && b != nil && b.Label == "TS-BARE-"+mode {
				t.Errorf("%s mode: the compatible finder handed out the bare cart", mode)
			}
		}
	}
}
