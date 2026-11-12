package engine

import (
	"fmt"
	"slices"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/engine/changeover"
	"shingoedge/release"
	"shingoedge/store/processes"
)

// tool_change_table_test.go — the tool change on every line mode, as a table:
// mode × layout × flip × the same or a new part × the same or a new carrier
// type × marks (none, some, all) × whether the incoming style leaves a position
// out (a drop). Every row is planned through the pipeline
// planChangeover runs (toolingTestPlanSeated) and checked against the rules,
// which read steps and never a swap mode's name:
//
//   - nothing is set down on a marked position before a tooling_done wait on
//     that leg;
//   - no leg lifts from one position of a marked cell and sets it down on
//     another (nothing indexes during a tool change);
//   - a marked position's lift has no wait in front of it (it goes at START),
//     drops included, and START does for it what a Ready press does: its
//     action locks the departing count (ClearsAtStart) and its leg asks for
//     curtain waits;
//   - an unmarked drop still waits for Ready, and START locks nothing for it;
//   - every unmarked position whose part changes is still planned;
//   - no order ends on a wait;
//   - a changeover with no marks has no tooling_done wait, and no builder
//     writes one: only the decorator does;
//   - the planner and the readiness card read one situation.

// tcCell is one row's cell: the outgoing and incoming claims, every node the
// plan may name, and the positions of the cell.
type tcCell struct {
	from, to  []processes.NodeClaim
	positions []string
	marked    []string
	// dropped are the positions the incoming style does not use.
	dropped []string
	nodes   []string
}

// tcCase is one row of the table.
type tcCase struct {
	mode     string
	layout   int  // positions: 1 for a single-node cell, 2 or 3 for a press, 2 for a sequential pair
	flip     bool // the index robot fetches (press-index only)
	samePart bool
	sameType bool
	marks    string // none, some, all
	// drop: the incoming style leaves the cell's last position out. A press of
	// three keeps two; any other cell has no incoming claim at all.
	drop bool
}

func (c tcCase) String() string {
	part, carrier := "new part", "new carrier"
	if c.samePart {
		part = "same part"
	}
	if c.sameType {
		carrier = "same carrier"
	}
	flip := ""
	if c.flip {
		flip = " flipped"
	}
	drop := ""
	if c.drop {
		drop = "/drops the last"
	}
	return fmt.Sprintf("%s/%d%s/%s/%s/marks %s%s", c.mode, c.layout, flip, part, carrier, c.marks, drop)
}

// tcClaim is a claim of mode at node with every field a changeover of that
// mode reads, and inbound staging (a claim with marks must name it).
func tcClaim(mode protocol.SwapMode, node, part string) processes.NodeClaim {
	role := protocol.ClaimRoleConsume
	if mode == protocol.SwapModeTwoRobotPressIndex || mode == protocol.SwapModeSequential {
		role = protocol.ClaimRoleProduce
	}
	return processes.NodeClaim{
		CoreNodeName:        node,
		Role:                role,
		SwapMode:            mode,
		PayloadCode:         part,
		InboundSource:       "SRC",
		InboundStaging:      "IN_STG",
		OutboundStaging:     "OUT_STG",
		OutboundDestination: "MARKET",
	}
}

// build makes the row's cell.
func (c tcCase) build() tcCell {
	toPart := "PART-B"
	if c.samePart {
		toPart = "PART-A"
	}
	var cell tcCell
	mark := func(claim *processes.NodeClaim, nodes []string) {
		switch c.marks {
		case "some":
			claim.ChangeoverEvacNodes = nodes[:1]
		case "all":
			claim.ChangeoverEvacNodes = nodes
		}
		cell.marked = append(cell.marked, claim.ChangeoverEvacNodes...)
	}
	switch c.mode {
	case "press":
		cell.positions = []string{"P1", "P2", "P3"}[:c.layout]
		from := tcClaim(protocol.SwapModeTwoRobotPressIndex, "P1", "PART-A")
		from.PairedCoreNode = "P2"
		if c.layout == 3 {
			from.SecondPairedCoreNode = "P3"
		}
		from.RefillBy = refillFor(c.flip)
		to := from
		to.PayloadCode, to.ChangeoverEvacNodes = toPart, nil
		mark(&from, cell.positions)
		cell.from, cell.to = []processes.NodeClaim{from}, []processes.NodeClaim{to}
		if c.drop && c.layout == 3 {
			cell.to[0].SecondPairedCoreNode = ""
			cell.dropped = []string{"P3"}
		}
	case "sequential":
		cell.positions = []string{"SA", "SB"}
		for _, pair := range [][2]string{{"SA", "SB"}, {"SB", "SA"}} {
			from := tcClaim(protocol.SwapModeSequential, pair[0], "PART-A")
			from.PairedCoreNode = pair[1]
			to := from
			to.PayloadCode = toPart
			if c.marks == "all" || (c.marks == "some" && pair[0] == "SA") {
				from.ChangeoverEvacNodes = []string{pair[0]}
				cell.marked = append(cell.marked, pair[0])
			}
			cell.from, cell.to = append(cell.from, from), append(cell.to, to)
		}
	default:
		cell.positions = []string{"N"}
		from := tcClaim(protocol.SwapMode(c.mode), "N", "PART-A")
		to := from
		to.PayloadCode = toPart
		if c.marks != "none" {
			mark(&from, cell.positions)
		}
		cell.from, cell.to = []processes.NodeClaim{from}, []processes.NodeClaim{to}
	}
	if c.drop && cell.dropped == nil {
		cell.to, cell.dropped = nil, slices.Clone(cell.positions)
	}
	cell.nodes = append(slices.Clone(cell.positions), "SRC", "IN_STG", "OUT_STG", "MARKET")
	return cell
}

// tcCases is the whole table.
func tcCases() []tcCase {
	type shape struct {
		mode   string
		layout int
		flip   bool
	}
	shapes := []shape{
		{string(protocol.SwapModeSingleRobot), 1, false},
		{string(protocol.SwapModeTwoRobot), 1, false},
		{"press", 2, false}, {"press", 2, true}, {"press", 3, false},
		{"sequential", 2, false},
	}
	var out []tcCase
	for _, s := range shapes {
		for _, samePart := range []bool{true, false} {
			for _, sameType := range []bool{true, false} {
				if samePart && !sameType {
					continue // a part rides its own carrier: the same part cannot change it
				}
				for _, marks := range []string{"none", "some", "all"} {
					if s.layout == 1 && marks == "some" {
						continue // one position: some is all
					}
					for _, drop := range []bool{false, true} {
						out = append(out, tcCase{s.mode, s.layout, s.flip, samePart, sameType, marks, drop})
					}
				}
			}
		}
	}
	return out
}

// tcBinTypes is the catalog a row reads: a new carrier is a different bin type
// for the incoming part.
func tcBinTypes(c tcCase) map[string]string {
	bt := map[string]string{"PART-A": "BT-1", "PART-B": "BT-1"}
	if !c.sameType {
		bt["PART-B"] = "BT-2"
	}
	return bt
}

// orderSteps is every order the plan creates, named for the failure message.
func orderSteps(p changeover.Plan) map[string][]protocol.ComplexOrderStep {
	out := map[string][]protocol.ComplexOrderStep{}
	for _, a := range p.Actions {
		for leg, spec := range map[string]*changeover.OrderSpec{"supply": a.SupplyOrder, "evac": a.EvacOrder} {
			if spec != nil && spec.Complex != nil {
				out[a.CoreNodeName+"/"+leg] = spec.Complex.Steps
			}
		}
	}
	return out
}

func isToolingWait(s protocol.ComplexOrderStep) bool {
	return s.Action == protocol.ActionWait && s.Purpose == string(release.PurposeToolingDone)
}

func TestToolChange_TheTable(t *testing.T) {
	t.Parallel()
	for _, c := range tcCases() {
		cell := c.build()
		plan := toolingTestPlanSeated(t, cell.from, cell.to, tcBinTypes(c), nil, cell.nodes...)
		name := c.String()
		inCell := func(n string) bool { return slices.Contains(cell.positions, n) }
		marked := func(n string) bool { return slices.Contains(cell.marked, n) }
		dropped := func(n string) bool { return slices.Contains(cell.dropped, n) }
		for _, a := range plan.Actions {
			if a.Err != nil {
				t.Errorf("%s: %s planned an error: %v", name, a.CoreNodeName, a.Err)
			}
		}
		setDown := map[string]bool{}
		for leg, steps := range orderSteps(plan) {
			if len(steps) > 0 && steps[len(steps)-1].Action == protocol.ActionWait {
				t.Errorf("%s: %s ends on a wait: %s", name, leg, stepsString(steps))
			}
			toolingSeen, carrying := false, ""
			for i, s := range steps {
				switch {
				case isToolingWait(s):
					toolingSeen = true
					if c.marks == "none" {
						t.Errorf("%s: %s waits for tooling with nothing marked: %s", name, leg, stepsString(steps))
					}
				case s.Action == protocol.ActionPickup:
					carrying = s.Node
					if marked(s.Node) && i > 0 && steps[i-1].Action == protocol.ActionWait {
						t.Errorf("%s: %s waits before lifting marked %s; a tool change lifts at START: %s",
							name, leg, s.Node, stepsString(steps))
					}
				case s.Action == protocol.ActionDropoff:
					setDown[s.Node] = true
					if marked(s.Node) && !toolingSeen {
						t.Errorf("%s: %s sets down on marked %s before tooling is done: %s", name, leg, s.Node, stepsString(steps))
					}
					if c.marks != "none" && inCell(carrying) && inCell(s.Node) && carrying != s.Node {
						t.Errorf("%s: %s indexes %s onto %s during a tool change: %s", name, leg, carrying, s.Node, stepsString(steps))
					}
					carrying = ""
				}
			}
		}
		// START does for a marked lift what a Ready press does, and an unmarked
		// drop still waits for its Ready.
		for _, a := range plan.Actions {
			for leg, spec := range map[string]*changeover.OrderSpec{"supply": a.SupplyOrder, "evac": a.EvacOrder} {
				if spec == nil || spec.Complex == nil {
					continue
				}
				steps := spec.Complex.Steps
				i := slices.IndexFunc(steps, func(s protocol.ComplexOrderStep) bool {
					return s.Action == protocol.ActionPickup && s.Node == a.CoreNodeName
				})
				if i < 0 {
					continue
				}
				waited := i > 0 && steps[i-1].Action == protocol.ActionWait
				switch {
				case marked(a.CoreNodeName) && !waited:
					if !a.ClearsAtStart {
						t.Errorf("%s: %s/%s lifts marked %s at START and does not lock its count there: %s",
							name, a.CoreNodeName, leg, a.CoreNodeName, stepsString(steps))
					}
					if !spec.Complex.CurtainWaits {
						t.Errorf("%s: %s/%s lifts marked %s at START and asks for no curtain wait: %s",
							name, a.CoreNodeName, leg, a.CoreNodeName, stepsString(steps))
					}
				case dropped(a.CoreNodeName) && !marked(a.CoreNodeName):
					if !waited || steps[i-1].Purpose != string(release.PurposeReady) {
						t.Errorf("%s: %s/%s drops unmarked %s with no Ready in front of the lift: %s",
							name, a.CoreNodeName, leg, a.CoreNodeName, stepsString(steps))
					}
					if a.ClearsAtStart {
						t.Errorf("%s: %s locks its count at START, but an unmarked drop waits for Ready", name, a.CoreNodeName)
					}
				}
			}
		}
		for _, p := range cell.dropped {
			if !slices.ContainsFunc(plan.Actions, func(a changeover.NodeAction) bool {
				return a.CoreNodeName == p && a.Situation == string(SituationDrop)
			}) {
				t.Errorf("%s: the incoming style leaves %s out and nothing drops it", name, p)
			}
		}
		if !c.samePart {
			for _, p := range cell.positions {
				if dropped(p) {
					continue
				}
				if !setDown[p] {
					t.Errorf("%s: nothing sets the new part down on %s", name, p)
				}
			}
		}
		// No builder writes a tooling wait: the plan without the decorator has none.
		diffs := DiffStyleClaims(cell.from, cell.to)
		diffs = PlanPressPositions(diffs, tcBinTypes(c), nil)
		nodes := make([]processes.Node, 0, len(cell.nodes))
		for i, n := range cell.nodes {
			nodes = append(nodes, processes.Node{ID: int64(i + 1), CoreNodeName: n, Name: n})
		}
		bare := BuildChangeoverPlan(diffs, nodes, false, nil, toolingChangeover{})
		for leg, steps := range orderSteps(bare) {
			if slices.ContainsFunc(steps, isToolingWait) {
				t.Errorf("%s: a builder wrote a tooling wait on %s: %s", name, leg, stepsString(steps))
			}
		}
		// The planner and the readiness card read one situation.
		for _, d := range DiffStyleClaims(cell.from, cell.to) {
			if want := domain.ChangeoverSituation(d.FromClaim, d.ToClaim); d.Situation != want {
				t.Errorf("%s: %s planned %s, the card reads %s", name, d.CoreNodeName, d.Situation, want)
			}
		}
	}
}
