package engine

import (
	"testing"

	"shingoedge/engine/changeover"
	"shingoedge/store/processes"
)

// changeover_bare_position_test.go — a press with a bare position, changed
// over.
//
// REQUEST has always answered a bare press position with a plain delivery to
// it and no swap (planVacantLine). The changeover read only the front's seat, so
// a press whose BACK was bare planned the press's own swap, whose index leg
// lifts at the back and holds there at Core for a bin that is not coming. Both
// paths now read vacantPositions: a press with any bare position is planned
// position by position, a bare position is just delivered to, and a held one
// gets its own swap.

func seatsAt(empty ...string) func(string) NodeState {
	bare := map[string]bool{}
	for _, n := range empty {
		bare[n] = true
	}
	return func(n string) NodeState {
		if bare[n] {
			return NodeState{Seat: SeatEmpty}
		}
		return NodeState{Seat: SeatBin}
	}
}

func planPress(state func(string) NodeState) map[string]ChangeoverSituation {
	from := crossModeFromPressIndex("PART-A", "FRONT", "MIDDLE", "BACK")
	to := crossModeFromPressIndex("PART-B", "FRONT", "MIDDLE", "BACK")
	diffs := []ChangeoverNodeDiff{{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to}}
	binTypes := map[string]string{"PART-A": "T1", "PART-B": "T1"} // one carrier: no bin-type split
	diffs = FanOutPressIndex(diffs, binTypes, state)
	diffs = FanOutPressIndexCrossMode(diffs, binTypes)
	diffs = ApplyNodeState(diffs, state)
	out := map[string]ChangeoverSituation{}
	for _, d := range diffs {
		out[d.CoreNodeName] = d.Situation
	}
	return out
}

func TestChangeoverPress_PlansEachPositionFromItsOwnSeat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		seat func(string) NodeState
		want map[string]ChangeoverSituation
	}{
		{"every position held: the press's own swap", seatsAt(),
			map[string]ChangeoverSituation{"FRONT": SituationSwap}},
		{"seats unknown: read held, the press's own swap", func(string) NodeState { return NodeState{} },
			map[string]ChangeoverSituation{"FRONT": SituationSwap}},
		{"back bare: deliver to it, swap the others", seatsAt("BACK"),
			map[string]ChangeoverSituation{"FRONT": SituationSwap, "MIDDLE": SituationSwap, "BACK": SituationAdd}},
		{"front bare: deliver to it, swap the others", seatsAt("FRONT"),
			map[string]ChangeoverSituation{"FRONT": SituationAdd, "MIDDLE": SituationSwap, "BACK": SituationSwap}},
		{"all bare: deliver to each", seatsAt("FRONT", "MIDDLE", "BACK"),
			map[string]ChangeoverSituation{"FRONT": SituationAdd, "MIDDLE": SituationAdd, "BACK": SituationAdd}},
	} {
		got := planPress(tc.seat)
		if len(got) != len(tc.want) {
			t.Errorf("%s: planned %v, want %v", tc.name, got, tc.want)
			continue
		}
		for n, s := range tc.want {
			if got[n] != s {
				t.Errorf("%s: %s planned %s, want %s (all: %v)", tc.name, n, got[n], s, got)
			}
		}
	}
}

// TestChangeoverPress_BarePositionGetsNoLift: the orders the planner builds
// for a press with a bare back never pick up at the back.
func TestChangeoverPress_BarePositionGetsNoLift(t *testing.T) {
	t.Parallel()
	from := crossModeFromPressIndex("PART-A", "FRONT", "MIDDLE", "BACK")
	to := crossModeFromPressIndex("PART-B", "FRONT", "MIDDLE", "BACK")
	state := seatsAt("BACK")
	diffs := []ChangeoverNodeDiff{{CoreNodeName: "FRONT", Situation: SituationSwap, FromClaim: &from, ToClaim: &to}}
	binTypes := map[string]string{"PART-A": "T1", "PART-B": "T1"}
	diffs = ApplyNodeState(FanOutPressIndexCrossMode(FanOutPressIndex(diffs, binTypes, state), binTypes), state)
	nodes := []processes.Node{{ID: 1, Name: "FRONT", CoreNodeName: "FRONT"},
		{ID: 2, Name: "MIDDLE", CoreNodeName: "MIDDLE"}, {ID: 3, Name: "BACK", CoreNodeName: "BACK"}}
	plan := BuildChangeoverPlan(diffs, nodes, false, nil, toolingChangeover{})
	for _, a := range plan.Actions {
		if a.Err != nil {
			t.Fatalf("%s: %v", a.CoreNodeName, a.Err)
		}
		for _, spec := range []*changeover.OrderSpec{a.SupplyOrder, a.EvacOrder} {
			if spec == nil || spec.Complex == nil {
				continue
			}
			for _, st := range spec.Complex.Steps {
				if st.Action == "pickup" && st.Node == "BACK" {
					t.Errorf("%s's order lifts at the bare BACK: %+v", a.CoreNodeName, spec.Complex.Steps)
				}
			}
		}
	}
}
