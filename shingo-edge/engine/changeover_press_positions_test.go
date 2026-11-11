package engine

import (
	"strings"
	"testing"

	"shingoedge/store/processes"
)

// changeover_press_positions_test.go — every position of a press, either
// style's, planned by one rule: a position only the incoming style uses is a
// plain delivery unless a bin is known to stand there.

// plannedPress runs the changeover's press passes and the node-state pass as
// planChangeover runs them, and answers each node's situation.
func plannedPress(from, to []processes.NodeClaim, binTypes map[string]string,
	state func(string) NodeState) map[string]ChangeoverSituation {
	diffs := PlanPressPositions(DiffStyleClaims(from, to), binTypes, state)
	diffs = ApplyNodeState(diffs, state)
	out := map[string]ChangeoverSituation{}
	for _, d := range diffs {
		out[d.CoreNodeName] = d.Situation
	}
	return out
}

// seatOnly answers seat at the named nodes and SeatUnknown elsewhere.
func seatOnly(seat Seat, names ...string) func(string) NodeState {
	return func(n string) NodeState {
		for _, name := range names {
			if n == name {
				return NodeState{Seat: seat}
			}
		}
		return NodeState{}
	}
}

// TestPressPositions_ANewBackPositionIsDeliveredToUnlessABinIsBound: a press
// moved to other nodes (the disjoint shape: the old style on one front and
// back, the new style on another). The new back has never had a carrier leave
// it, so its seat is unknown, and it is an Add: a swap there lifts first, and at
// Core a lift at a node with nothing on it holds the swap's reserve with no
// limit. Only a bin known to be bound there makes it the swap.
func TestPressPositions_ANewBackPositionIsDeliveredToUnlessABinIsBound(t *testing.T) {
	t.Parallel()
	from := pressClaim("PLN_001", "PLN_002", "PART-A")
	to := pressClaim("PLN_005", "PLN_006", "PART-A")
	binTypes := map[string]string{"PART-A": "BIG"}
	for _, c := range []struct {
		name  string
		state func(string) NodeState
		back  ChangeoverSituation
		front ChangeoverSituation
	}{
		{"no seat read", nil, SituationAdd, SituationAdd},
		{"seats unknown: never a carrier recorded", seatOnly(SeatUnknown), SituationAdd, SituationAdd},
		{"the new back's carrier recorded gone", seatOnly(SeatEmpty, "PLN_006"), SituationAdd, SituationAdd},
		{"a bin bound on the new back", seatOnly(SeatBin, "PLN_006"), SituationSwap, SituationAdd},
		{"a bin bound on the new front", seatOnly(SeatBin, "PLN_005"), SituationAdd, SituationSwap},
	} {
		got := plannedPress([]processes.NodeClaim{from}, []processes.NodeClaim{to}, binTypes, c.state)
		want := map[string]ChangeoverSituation{"PLN_001": SituationDrop, "PLN_002": SituationDrop,
			"PLN_005": c.front, "PLN_006": c.back}
		if len(got) != len(want) {
			t.Errorf("%s: planned %v, want %v", c.name, got, want)
			continue
		}
		for n, s := range want {
			if got[n] != s {
				t.Errorf("%s: %s planned %s, want %s (all: %v)", c.name, n, got[n], s, got)
			}
		}
	}
}

// TestPressPositions_APositionAddedToTheSamePressIsDeliveredTo: a press that
// gains a position on the same front, the carrier type unchanged, so the press
// is not split: the added position is an Add unless a bin is bound there.
func TestPressPositions_APositionAddedToTheSamePressIsDeliveredTo(t *testing.T) {
	t.Parallel()
	from := crossModeFromPressIndex("PART-A", "FRONT", "MIDDLE", "")
	to := crossModeFromPressIndex("PART-B", "FRONT", "MIDDLE", "BACK")
	binTypes := map[string]string{"PART-A": "T1", "PART-B": "T1"}
	held := func(string) NodeState { return NodeState{Seat: SeatBin} }
	for _, c := range []struct {
		name  string
		state func(string) NodeState
		want  map[string]ChangeoverSituation
	}{
		{"every seat unknown", seatOnly(SeatUnknown),
			map[string]ChangeoverSituation{"FRONT": SituationSwap, "BACK": SituationAdd}},
		{"every seat holding a bin", held,
			map[string]ChangeoverSituation{"FRONT": SituationSwap, "BACK": SituationSwap}},
	} {
		got := plannedPress([]processes.NodeClaim{from}, []processes.NodeClaim{to}, binTypes, c.state)
		if len(got) != len(c.want) {
			t.Errorf("%s: planned %v, want %v", c.name, got, c.want)
			continue
		}
		for n, s := range c.want {
			if got[n] != s {
				t.Errorf("%s: %s planned %s, want %s (all: %v)", c.name, n, got[n], s, got)
			}
		}
	}
}

// TestPressPositions_TheNewBackGetsNoLift: the orders the planner builds for
// the disjoint shape with nothing known on the new back never pick up there.
func TestPressPositions_TheNewBackGetsNoLift(t *testing.T) {
	t.Parallel()
	from := pressClaim("PLN_001", "PLN_002", "PART-A")
	to := pressClaim("PLN_005", "PLN_006", "PART-A")
	plan := toolingTestPlanSeated(t, []processes.NodeClaim{from}, []processes.NodeClaim{to},
		map[string]string{"PART-A": "BIG"}, seatOnly(SeatUnknown), "PLN_001", "PLN_002", "PLN_005", "PLN_006")
	a := actionFor(t, plan, "PLN_006")
	if a.Situation != string(SituationAdd) {
		t.Errorf("PLN_006 planned %s, want add", a.Situation)
	}
	if got := allSteps(a); strings.Contains(got, "pickup@PLN_006") {
		t.Errorf("PLN_006's order lifts at a position nothing is known to stand on: %s", got)
	}
}
