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
	// A few minutes in, this is a swap in progress. It says what is happening
	// and asks for nothing.
	if !strings.HasPrefix(detail, "Binding in progress — UOP accumulating.") {
		t.Errorf("detail = %q, want it to open by naming what is happening", detail)
	}
	if strings.Contains(detail, "Record Count") {
		t.Errorf("detail = %q — a few minutes into a swap is the NORMAL case; "+
			"asking the operator to intervene here is what taught them the notice means nothing", detail)
	}
	// The elapsed time has to be a number that says something. Whole hours meant
	// every notice inside the first hour read "staged 0h".
	if strings.Contains(detail, "0h") {
		t.Errorf("detail = %q renders the wait as 0h; below an hour it must render minutes", detail)
	}
	if !strings.Contains(detail, "min") {
		t.Errorf("detail = %q, want the wait in minutes", detail)
	}
	if got := got[0]; got.NeedsAction {
		t.Error("event says needs_action for a swap that has been running four minutes")
	}

	// Binding clears the alarm.
	bound := int64(99)
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &bound), "bind a bin")
	sm.evaluate(node, base.Add(time.Duration(strandedWindow+3)*time.Minute))
	if d := eng.StrandedAlarmDetail(node.CoreNodeName); d != "" {
		t.Errorf("after bind, alarm detail = %q, want cleared", d)
	}
}

// TestStrandedDetail_AsksOnlyOnceTheWaitIsLong pins the two sentences and the
// line between them.
//
// The chip used to open with "Record Count on the bin tab" from the first
// second, which put an alarm face on the ordinary case — a carrier at the line
// during a swap, counts held until the bind lands, exactly what is supposed to
// happen. An alarm that is usually nothing is ignored when it is something. The
// call to action now waits until the gap is longer than a swap takes.
func TestStrandedDetail_AsksOnlyOnceTheWaitIsLong(t *testing.T) {
	t.Parallel()
	const ctaAfter = 30 * time.Minute
	for _, tc := range []struct {
		name    string
		unbound time.Duration
		wantCTA bool
		wantIn  string
	}{
		{"seconds in", 20 * time.Second, false, "just now"},
		{"one minute", time.Minute, false, "1 min"},
		{"mid swap", 12 * time.Minute, false, "12 min"},
		{"one minute short", 29 * time.Minute, false, "29 min"},
		{"at the limit", 30 * time.Minute, true, "30 min"},
		{"well past", 95 * time.Minute, true, "1h 35m"},
		{"exactly two hours", 2 * time.Hour, true, "2 hr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatStrandedDetail("BIN-7", tc.unbound, ctaAfter, "LINE1-IN")
			if asks := strings.Contains(got, "Record Count"); asks != tc.wantCTA {
				t.Errorf("after %s: asks for action = %v, want %v\n  %s", tc.unbound, asks, tc.wantCTA, got)
			}
			if !strings.Contains(got, tc.wantIn) {
				t.Errorf("after %s: want the wait rendered as %q\n  %s", tc.unbound, tc.wantIn, got)
			}
			if strings.Contains(got, "0h") {
				t.Errorf("after %s: rendered as 0h, which tells the operator nothing\n  %s", tc.unbound, got)
			}
			if !strings.HasPrefix(got, "Binding in progress — UOP accumulating.") {
				t.Errorf("after %s: want the sentence to open by naming what is happening\n  %s", tc.unbound, got)
			}
		})
	}
}
