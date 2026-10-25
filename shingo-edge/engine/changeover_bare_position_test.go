package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
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

// TestChangeoverPress_AVacantOnDeckIsBroughtACarrierCoreSeesComing is the
// verify-first of the one bind rule (press tranche 2, phase E), per press-index
// mode: a changeover into a press whose on-deck is vacant plans, for each vacant
// dependent position, an order Core reads as coming to it and no leg lifting
// from it. Core's vacant-pickup fallback asks orders.CarrierComingTo before it
// fetches, and that read is protocol.OrderPlacesBinAt over each live row as Core
// stores it (a plain order by its delivery node, a complex one by its steps),
// with only the asker's partner left out; an Add is no leg's partner. So with
// the index leg's on-deck pickup naming no part, the fallback never fetches a
// second carrier for a changeover's own Add, in any mode.
//
// A seat that cannot say builds no Add: the press's own swap, whose index leg
// lifts the on-deck and whose partner's refill is the pair's own (never waited
// on), so the fallback fetches at most the one carrier the index needs.
func TestChangeoverPress_AVacantOnDeckIsBroughtACarrierCoreSeesComing(t *testing.T) {
	t.Parallel()
	type shape struct {
		name    string
		from    func(role protocol.ClaimRole, flip bool, second string) processes.NodeClaim
		carrier map[string]string
	}
	press := func(part string) func(protocol.ClaimRole, bool, string) processes.NodeClaim {
		return func(role protocol.ClaimRole, flip bool, second string) processes.NodeClaim {
			c := pressClaim("PRESS", "DECK", part)
			c.Role, c.IndexRobotSupplies, c.SecondPairedCoreNode = role, flip, second
			return c
		}
	}
	tooling := func(role protocol.ClaimRole, flip bool, second string) processes.NodeClaim {
		c := press("PART-A")(role, flip, second)
		c.ChangeoverEvacNodes = c.Positions()
		return c
	}
	crossFrom := func(mode protocol.SwapMode) func(protocol.ClaimRole, bool, string) processes.NodeClaim {
		return func(role protocol.ClaimRole, _ bool, _ string) processes.NodeClaim {
			c := crossModeNonPressIndex("PRESS", "PART-A", mode)
			c.Role = role
			return c
		}
	}
	same := map[string]string{"PART-A": "T1", "PART-B": "T1"}
	other := map[string]string{"PART-A": "T1", "PART-B": "T2"}
	shapes := []shape{
		{"press to press, one carrier", press("PART-A"), same},
		{"press to press, another carrier", press("PART-A"), other},
		{"press to press, tooling", tooling, same},
		{"two_robot to press", crossFrom(protocol.SwapModeTwoRobot), same},
		{"single_robot to press", crossFrom(protocol.SwapModeSingleRobot), same},
	}
	for _, sh := range shapes {
		for _, role := range []protocol.ClaimRole{protocol.ClaimRoleProduce, protocol.ClaimRoleConsume} {
			for _, flip := range []bool{false, true} {
				for _, second := range []string{"", "DECK2"} {
					from := sh.from(role, flip, second)
					to := press("PART-B")(role, flip, second)
					to.InboundStaging = "IN-STAGE"
					deps := to.DependentPositions()
					name := fmt.Sprintf("%s/%s/flip=%v/positions=%d", sh.name, role, flip, 1+len(deps))
					for _, vacant := range append([][]string{{}}, vacantSets(deps)...) {
						seat := map[string]Seat{"PRESS": SeatBin}
						for _, d := range deps {
							seat[d] = SeatUnknown
						}
						for _, v := range vacant {
							seat[v] = SeatEmpty
						}
						plan := toolingTestPlanSeated(t, []processes.NodeClaim{from}, []processes.NodeClaim{to},
							sh.carrier, seats(seat), append([]string{"PRESS"}, deps...)...)
						assertVacantOnDeckBrought(t, fmt.Sprintf("%s/vacant=%v", name, vacant), plan, deps, vacant)
					}
				}
			}
		}
	}
}

// vacantSets is every non-empty subset of the dependent positions.
func vacantSets(deps []string) [][]string {
	var out [][]string
	for mask := 1; mask < 1<<len(deps); mask++ {
		var set []string
		for i, d := range deps {
			if mask&(1<<i) != 0 {
				set = append(set, d)
			}
		}
		out = append(out, set)
	}
	return out
}

// assertVacantOnDeckBrought says, of a changeover plan, that each vacant
// dependent position has an order Core reads as coming to it that is no leg
// lifting from it, and that nothing lifts from it; with no vacant position (a
// seat that cannot say), that no plain order is brought to any of them.
func assertVacantOnDeckBrought(t *testing.T, name string, plan changeover.Plan, deps, vacant []string) {
	t.Helper()
	type row struct {
		orderType protocol.OrderType
		steps     []protocol.ComplexOrderStep
		delivery  string
	}
	var rows []row
	for _, a := range plan.Actions {
		if a.Err != nil {
			t.Fatalf("%s: %s: %v", name, a.CoreNodeName, a.Err)
		}
		for _, spec := range []*changeover.OrderSpec{a.SupplyOrder, a.EvacOrder} {
			switch {
			case spec == nil:
			case spec.Retrieve != nil:
				ot := protocol.OrderTypeRetrieve
				if spec.Retrieve.RetrieveEmpty {
					ot = protocol.OrderTypeRetrieveEmpty
				}
				rows = append(rows, row{ot, nil, spec.Retrieve.DeliveryNode})
			case spec.Complex != nil:
				rows = append(rows, row{protocol.OrderTypeComplex, spec.Complex.Steps, spec.Complex.DeliveryNode})
			}
		}
	}
	lifts := func(r row, node string) bool {
		for _, s := range r.steps {
			if s.Action == protocol.ActionPickup && s.Node == node {
				return true
			}
		}
		return false
	}
	isVacant := map[string]bool{}
	for _, v := range vacant {
		isVacant[v] = true
		coming := 0
		for _, r := range rows {
			if lifts(r, v) {
				t.Errorf("%s: an order lifts at the vacant %s: %s %+v", name, v, r.orderType, r.steps)
			}
			if protocol.OrderPlacesBinAt(r.orderType, r.steps, r.delivery, v) {
				coming++
			}
		}
		if coming == 0 {
			t.Errorf("%s: nothing Core reads as coming to the vacant %s; the fallback would fetch one (orders %+v)",
				name, v, rows)
		}
	}
	if len(vacant) > 0 {
		return
	}
	for _, d := range deps {
		for _, r := range rows {
			if r.orderType != protocol.OrderTypeComplex && r.delivery == d {
				t.Errorf("%s: a seat that cannot say is brought a plain order at %s: an Add the press's own swap "+
					"would collide with", name, d)
			}
		}
	}
}
