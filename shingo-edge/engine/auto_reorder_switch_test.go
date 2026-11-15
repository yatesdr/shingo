package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// auto_reorder_switch_test.go — a line's auto-reorder switch (auto_reorder_switch.go).

// TestFlipAutoReorder_OffAndOnAtEveryPosition: off is a person's off at every
// position of the line, over any hold, with who as its note; on clears every
// hold, a person's off included, and stamps the go; each flip is recorded,
// newest first, and the sweep does not ask while it is off.
func TestFlipAutoReorder_OffAndOnAtEveryPosition(t *testing.T) {
	t.Parallel()
	l := newIdleLine(t, time.Minute)
	second, err := l.db.CreateProcessNode(processes.NodeInput{
		ProcessID: l.processID, CoreNodeName: "ALN_IDLE_B", Code: "I2", Name: "ALN_IDLE_B", Sequence: 2, Enabled: true,
	})
	testutil.MustNoErr(t, err, "a second position")
	_, err = l.db.EnsureProcessNodeRuntime(second)
	testutil.MustNoErr(t, err, "its runtime row")
	_, err = processes.HoldReorder(l.db.DB, l.nodeID, domain.ReorderHoldOff, time.Now(), "idle")
	testutil.MustNoErr(t, err, "an off first")

	testutil.MustNoErr(t, l.eng.FlipAutoReorder(l.processID, false, "Pat", domain.FlipFromProcesses), "switch off")
	for _, id := range []int64{l.nodeID, second} {
		rt, err := l.db.GetProcessNodeRuntime(id)
		testutil.MustNoErr(t, err, "read a position")
		if rt.ReorderHold != domain.ReorderHoldPersonOff || rt.ReorderHoldNote != "Pat" {
			t.Errorf("position %d after the switch off: %q by %q, want person_off by Pat", id, rt.ReorderHold, rt.ReorderHoldNote)
		}
	}
	l.eng.sweepCellLevels()
	if got := countOrders(t, l.db); got != 0 {
		t.Errorf("the sweep asked %d orders with the line switched off", got)
	}
	before := time.Now().Add(-time.Second)
	testutil.MustNoErr(t, l.eng.FlipAutoReorder(l.processID, true, "Sam", domain.FlipFromProcesses), "switch on")
	rt, err := l.db.GetProcessNodeRuntime(l.nodeID)
	testutil.MustNoErr(t, err, "read the position")
	if rt.ReorderHold != domain.ReorderHoldNone || rt.ReorderGoAt.Before(before) {
		t.Errorf("after the switch on: hold %q, go %v; want none and a go now", rt.ReorderHold, rt.ReorderGoAt)
	}
	flips, err := processes.ListAutoReorderFlips(l.db.DB, l.processID, 10)
	testutil.MustNoErr(t, err, "read the flips")
	if len(flips) != 2 || !flips[0].TurnedOn || flips[0].By != "Sam" || flips[1].TurnedOn || flips[1].By != "Pat" ||
		flips[0].Source != domain.FlipFromProcesses || flips[0].At.IsZero() {
		t.Errorf("flips = %+v, want Sam's on then Pat's off, from Processes, stamped", flips)
	}
}

// TestFlipAutoReorder_TheRefusals: turning it on needs a counter configured and
// read since boot; turning it off needs neither; the station flips only where
// Processes shows the switch there.
func TestFlipAutoReorder_TheRefusals(t *testing.T) {
	t.Parallel()
	t.Run("no counter configured", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, _ := keeperFixture(t)
		node, err := db.GetProcessNode(nodeID)
		testutil.MustNoErr(t, err, "read the node")
		if err := eng.FlipAutoReorder(node.ProcessID, true, "Pat", domain.FlipFromProcesses); !errors.Is(err, ErrAutoReorderNoCounter) {
			t.Errorf("on with no counter: %v, want ErrAutoReorderNoCounter", err)
		}
		testutil.MustNoErr(t, eng.FlipAutoReorder(node.ProcessID, false, "Pat", domain.FlipFromProcesses), "off needs no counter")
	})
	t.Run("a counter not read since boot", func(t *testing.T) {
		t.Parallel()
		l := newIdleLine(t, time.Minute)
		l.eng.stationHealth = stationHealthTracker{}
		err := l.eng.FlipAutoReorder(l.processID, true, "Pat", domain.FlipFromProcesses)
		if err == nil || !strings.Contains(err.Error(), "cannot be read now") {
			t.Errorf("on with an unread counter: %v, want refused as unreadable", err)
		}
	})
	t.Run("the station, where it is not shown", func(t *testing.T) {
		t.Parallel()
		l := newIdleLine(t, time.Minute)
		if err := l.eng.FlipAutoReorder(l.processID, false, "LINE-OPS", domain.FlipFromStation); !errors.Is(err, ErrAutoReorderSwitchHidden) {
			t.Errorf("station flip with the switch hidden: %v, want ErrAutoReorderSwitchHidden", err)
		}
		testutil.MustNoErr(t, processes.SetAutoReorderSwitchHMI(l.db.DB, l.processID, true), "show it")
		testutil.MustNoErr(t, l.eng.FlipAutoReorder(l.processID, false, "LINE-OPS", domain.FlipFromStation), "station flip, shown")
	})
}

// TestArming_ProcessesRefusesTurningItOnWithNoCounter: a save that turns a
// claim's auto-reorder on in a line with no counter is refused with the
// finding; at aea6321d it saved.
func TestArming_ProcessesRefusesTurningItOnWithNoCounter(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claimID := keeperFixture(t)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read the node")
	_, err = db.DB.Exec(`UPDATE style_node_claims SET auto_reorder=0 WHERE id=?`, claimID)
	testutil.MustNoErr(t, err, "turn the claim off")
	claim, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "read the claim")
	cell := domain.Collapse(*claim)
	cell.Advanced = domain.AdvancedOf(claim)
	cell.Advanced.AutoReorder = true
	fp, err := eng.FlowFingerprint(node.ProcessID, claim.StyleID)
	testutil.MustNoErr(t, err, "fingerprint")
	_, err = eng.SaveFlow(node.ProcessID, FlowSaveRequest{ToStyleID: claim.StyleID, Fingerprint: fp,
		Source: domain.ClaimSourceAdmin, CalledBy: "test", Cells: []domain.FlowCell{cell}})
	var refused *FlowValidationError
	if !errors.As(err, &refused) || refused.Findings[0].Message != ErrAutoReorderNoCounter.Error() {
		t.Fatalf("save arming a line with no counter: %v, want refused with the counter finding", err)
	}
	after, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "read the claim again")
	if after.AutoReorder {
		t.Error("the refused save armed the claim")
	}
}

// TestBuildView_TheSwitchShowsWhereProcessesAllowsIt: the board carries the
// line's switch only where Processes shows it, on until a person switches the
// line off.
func TestBuildView_TheSwitchShowsWhereProcessesAllowsIt(t *testing.T) {
	t.Parallel()
	l := newIdleLine(t, time.Minute)
	sid, err := l.db.CreateOperatorStation(stations.Input{ProcessID: l.processID, Name: "IDLE-STATION"})
	testutil.MustNoErr(t, err, "create station")
	_, err = l.db.DB.Exec(`UPDATE process_nodes SET operator_station_id=? WHERE id=?`, sid, l.nodeID)
	testutil.MustNoErr(t, err, "seat the node")
	processes.BumpNodeGeneration()
	view := func() *domain.StationAutoReorderSwitch {
		v, err := l.eng.stationService.BuildView(context.Background(), sid)
		testutil.MustNoErr(t, err, "build the view")
		return v.AutoReorderSwitch
	}
	if got := view(); got != nil {
		t.Fatalf("the board carries the switch (%+v) before Processes shows it", got)
	}
	testutil.MustNoErr(t, processes.SetAutoReorderSwitchHMI(l.db.DB, l.processID, true), "show it")
	if got := view(); got == nil || !got.On {
		t.Fatalf("shown and nothing switched off: %+v, want on", got)
	}
	testutil.MustNoErr(t, l.eng.FlipAutoReorder(l.processID, false, "Pat", domain.FlipFromProcesses), "switch off")
	if got := view(); got == nil || got.On {
		t.Errorf("switched off: %+v, want off", got)
	}
}
