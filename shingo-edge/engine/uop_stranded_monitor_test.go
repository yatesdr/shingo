package engine

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
)

// feed drives a sequence of (pending, bound) samples through the detector and
// returns how many times it fired and whether it ended active.
func feed(t *testing.T, samples []struct {
	pending int
	bound   bool
}) (fires int, endActive bool) {
	t.Helper()
	st := &strandedNodeState{}
	base := time.Now()
	for i, s := range samples {
		fire, active := evalStrandedGrowth(st, s.pending, s.bound, base.Add(time.Duration(i)*time.Minute))
		if fire {
			fires++
		}
		endActive = active
	}
	return fires, endActive
}

func TestEvalStrandedGrowth_SustainedGrowthFiresOnce(t *testing.T) {
	t.Parallel()
	// strandedWindow rises => strandedWindow+1 increasing samples fire once, and
	// continued growth stays active without re-firing.
	fires, active := feed(t, []struct {
		pending int
		bound   bool
	}{
		{10, false}, {20, false}, {30, false}, {40, false}, {50, false}, {60, false},
	})
	if fires != 1 {
		t.Errorf("fires = %d, want 1 (one alarm per growth window)", fires)
	}
	if !active {
		t.Error("want still active while pending keeps climbing unbound")
	}
}

func TestEvalStrandedGrowth_IdleLineNeverAlarms(t *testing.T) {
	t.Parallel()
	// Flat pending = an idle line: ticks are not arriving, so it must never alarm
	// no matter how large or how long.
	fires, active := feed(t, []struct {
		pending int
		bound   bool
	}{
		{500, false}, {500, false}, {500, false}, {500, false}, {500, false},
	})
	if fires != 0 || active {
		t.Errorf("idle line alarmed: fires=%d active=%v, want 0/false (no fixed threshold, growth only)", fires, active)
	}
}

func TestEvalStrandedGrowth_BoundNeverAlarms(t *testing.T) {
	t.Parallel()
	// A bound node is not stranded — growth against a bound bin is normal drain.
	fires, active := feed(t, []struct {
		pending int
		bound   bool
	}{
		{10, true}, {20, true}, {30, true}, {40, true}, {50, true},
	})
	if fires != 0 || active {
		t.Errorf("bound node alarmed: fires=%d active=%v, want 0/false", fires, active)
	}
}

func TestEvalStrandedGrowth_BindClearsMidWindow(t *testing.T) {
	t.Parallel()
	// Growth toward an alarm, then a bin binds: the window resets, no alarm.
	fires, active := feed(t, []struct {
		pending int
		bound   bool
	}{
		{10, false}, {20, false}, {30, false}, // climbing, not yet fired
		{0, true}, // bound — pending cleared onto the bin, window resets
	})
	if fires != 0 || active {
		t.Errorf("bind mid-climb alarmed: fires=%d active=%v, want 0/false (binding is the fix)", fires, active)
	}
}

func TestEvalStrandedGrowth_StallReArmsOnRenewedGrowth(t *testing.T) {
	t.Parallel()
	// Fire once, go flat (alarm clears — idle), then a renewed sustained climb
	// fires a second window. Proves the per-window dedup and the re-arm.
	fires, active := feed(t, []struct {
		pending int
		bound   bool
	}{
		{10, false}, {20, false}, {30, false}, {40, false}, // fire #1
		{40, false}, {40, false}, // flat: clears
		{50, false}, {60, false}, {70, false}, {80, false}, // fire #2
	})
	if fires != 2 {
		t.Errorf("fires = %d, want 2 (one per growth window, re-armed after the flat break)", fires)
	}
	if !active {
		t.Error("want active at the end of the second climb")
	}
}

// TestStrandedMonitor_Evaluate_FiresNamedAlarm drives the live evaluate() over a
// seeded consume node whose pending_uop_delta climbs while unbound, and asserts
// the exact operator sentence lands in the engine's alarm map + on the bus.
func TestStrandedMonitor_Evaluate_FiresNamedAlarm(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "PARKED", PayloadCode: "PART-PK", UOPCapacity: 200, InitialUOP: 0,
	})
	// Unbound slot: the SNF3 staged-but-unbound window.
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil), "clear active bin")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")

	eng := testEngine(t, db)
	var got []UOPStrandedEvent
	eng.Events.SubscribeTypes(func(evt Event) {
		if a, ok := evt.Payload.(UOPStrandedEvent); ok {
			got = append(got, a)
		}
	}, EventUOPStranded)

	sm := &strandedMonitor{eng: eng, states: map[int64]*strandedNodeState{}}
	base := time.Now()
	// Climb pending across strandedWindow+2 scans while unbound.
	for i := 0; i < strandedWindow+2; i++ {
		testutil.MustNoErr(t, db.AddPendingUOPDelta(nodeID, 7), "add pending")
		sm.evaluate(node, base.Add(time.Duration(i)*time.Minute))
	}

	if len(got) != 1 {
		t.Fatalf("EventUOPStranded fired %d times, want exactly 1 per window: %+v", len(got), got)
	}
	detail := eng.StrandedAlarmDetail(node.CoreNodeName)
	if detail == "" {
		t.Fatal("StrandedAlarmDetail empty, want the tile sentence")
	}
	// Exact operator text (carrier unresolved in test → generic subject).
	wantTail := "at " + node.CoreNodeName + ", not bound — Record Count on the bin tab."
	if !strings.HasSuffix(detail, wantTail) {
		t.Errorf("detail = %q, want it to end with %q", detail, wantTail)
	}
	if !strings.Contains(detail, "staged") {
		t.Errorf("detail = %q, want the 'staged Nh' phrasing", detail)
	}

	// Binding clears the alarm.
	bound := int64(99)
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &bound), "bind a bin")
	sm.evaluate(node, base.Add(time.Duration(strandedWindow+3)*time.Minute))
	if d := eng.StrandedAlarmDetail(node.CoreNodeName); d != "" {
		t.Errorf("after bind, alarm detail = %q, want cleared", d)
	}
}
