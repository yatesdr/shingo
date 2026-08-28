package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// stepTrace renders a step list as "action@node" so an assertion failure shows
// the whole choreography instead of one mismatched index.
func stepTrace(steps []protocol.ComplexOrderStep) string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		n := s.Node
		if n == "" {
			n = "-"
		}
		out = append(out, string(s.Action)+"@"+n)
	}
	return strings.Join(out, " ")
}

// positionPress builds the marked press whose position the decorator expands. These
// used to be two synthesized per-position claims handed to a builder; the
// decorator synthesizes them itself from the PARENT claims, which is what makes
// the marks reachable no matter what the earlier passes did to the diffs.
func positionPress() toolingPress {
	from := &processes.NodeClaim{
		CoreNodeName:              "PRESS",
		Role:                      protocol.ClaimRoleProduce,
		SwapMode:                  protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:               "PART-A",
		PairedCoreNode:            "PRESS_B",
		OutboundDestination:       "MARKET",
		ChangeoverEvacDestination: "TOOLING-BAY",
		ChangeoverEvacNodes:       []string{"PRESS_B"},
	}
	to := &processes.NodeClaim{
		CoreNodeName:   "PRESS",
		Role:           protocol.ClaimRoleProduce,
		SwapMode:       protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:    "PART-B",
		PairedCoreNode: "PRESS_B",
		InboundSource:  "EMPTIES",
		InboundStaging: "IN-STAGE",
	}
	return toolingPress{
		from: from, to: to,
		positions: []string{"PRESS_B"},
		evacDest:  "TOOLING-BAY",
		staging:   "IN-STAGE",
	}
}

func positionSteps(t *testing.T, press toolingPress, position string) []protocol.ComplexOrderStep {
	t.Helper()
	a := toolingPositionAction(press, position, &processes.Node{ID: 1, CoreNodeName: position, Name: position})
	if a.SupplyOrder == nil || a.SupplyOrder.Complex == nil {
		t.Fatalf("position %s got no complex supply order", position)
	}
	return a.SupplyOrder.Complex.Steps
}

// The staged shape: lift the blocking bin off the position, get it off the line,
// fetch the replacement, PARK AT STAGING holding it, and deliver on release.
func TestToolingPositionAction_Shape(t *testing.T) {
	t.Parallel()
	press := positionPress()
	a := toolingPositionAction(press, "PRESS_B", &processes.Node{ID: 1, CoreNodeName: "PRESS_B", Name: "PRESS_B"})

	want := "pickup@PRESS_B dropoff@TOOLING-BAY pickup@EMPTIES wait@IN-STAGE dropoff@PRESS_B"
	if got := stepTrace(a.SupplyOrder.Complex.Steps); got != want {
		t.Errorf("steps =\n  %s\nwant\n  %s", got, want)
	}
	if a.EvacOrder != nil {
		t.Error("a position is one robot; the decorator already split the cell")
	}
	// The order opens by lifting the OLD bin, so it must carry the from-style
	// payload — otherwise the pickup filters for the new payload and finds
	// nothing at the position (the ALN_001 shape).
	if got := a.SupplyOrder.Complex.PayloadCode; got != "PART-A" {
		t.Errorf("PayloadCode = %q, want the FROM-style PART-A", got)
	}
	if a.LogTag != "evacuate_staged_position" {
		t.Errorf("LogTag = %q, want evacuate_staged_position", a.LogTag)
	}
}

// Blank ChangeoverEvacDestination means NORMAL ROUTING, and the two halves of
// the pass want that answered differently.
//
// A leg the pipeline already planned is left alone — nothing to redirect, so
// nothing queued. A position this pass EXPANDS has no leg yet, so it still has to
// name somewhere for the bin to go, and normal routing for a press is its own
// outbound destination. One field, two readings, and the reason they differ is
// that only one of them is an edit.
func TestToolingChangeover_BlankDestinationMeansNormalRouting(t *testing.T) {
	t.Parallel()
	press := positionPress()
	from := *press.from
	from.ChangeoverEvacDestination = ""
	to := *press.to

	tc := planToolingChangeover([]processes.NodeClaim{from}, []processes.NodeClaim{to})
	if got, ok := tc.evacDest["PRESS_B"]; ok {
		t.Errorf("blank evac destination queued a redirect to %q; it must queue nothing", got)
	}
	if len(tc.presses) != 1 {
		t.Fatalf("expected one marked press, got %d", len(tc.presses))
	}
	if got := tc.presses[0].evacDest; got != "MARKET" {
		t.Errorf("an expanded position has no leg to leave alone, so it must be built against "+
			"OutboundDestination; got %q", got)
	}
}

// A produce position's replacement is a fresh EMPTY carrier. Without the flag the
// dispatch hunts a full payload-matched bin in the empties pool and fails.
func TestToolingPositionAction_ProduceFetchesAnEmpty(t *testing.T) {
	t.Parallel()
	press := positionPress()
	var found bool
	for _, s := range positionSteps(t, press, "PRESS_B") {
		if s.Action == protocol.ActionPickup && s.Node == "EMPTIES" {
			found = true
			if !s.Empty {
				t.Error("the InboundSource pickup on a produce position must be flagged Empty")
			}
		}
	}
	if !found {
		t.Fatal("no pickup at the inbound source")
	}

	// A consume position pulls a full bin, so the flag must NOT be set — otherwise
	// it fetches an empty carrier onto a line that needs parts.
	consume := positionPress()
	toConsume := *consume.to
	toConsume.Role = protocol.ClaimRoleConsume
	consume.to = &toConsume
	for _, s := range positionSteps(t, consume, "PRESS_B") {
		if s.Action == protocol.ActionPickup && s.Node == "EMPTIES" && s.Empty {
			t.Error("a consume position must fetch a FULL bin, not an empty carrier")
		}
	}
}

// SEQUENTIAL IS UNCHANGED BY THE DECORATOR. Both shapes still share
// buildToolingEvacSteps, and sequential's bare wait is its own deliberate
// choice — a regression here would be a shared helper quietly changing a mode
// nobody was working on.
//
// PER-NODE (2026-08-28): this used to call one builder and read StepsA and
// StepsB, because sequential evacuate returned BOTH positions from a single
// call. It is now one call per position, each returning its own StepsA. Same
// two step-lists, same bare wait, same assertion — the shape they arrive in is
// what changed, so the fixture follows rather than the claim.
func TestBuildSequentialPerPositionEvacuate_StillCarriesAcrossABareWait(t *testing.T) {
	t.Parallel()
	// InboundStaging IS SET, and unused by this builder. That is the point:
	// with it blank a staged wait leaking in through the shared helper would
	// still render as a bare wait and this test could not tell.
	to := &processes.NodeClaim{CoreNodeName: "SEQ-A", InboundSource: "MARKET", InboundStaging: "SEQ-STAGE"}
	claimAt := func(own, partner string) *processes.NodeClaim {
		return &processes.NodeClaim{
			CoreNodeName: own, PairedCoreNode: partner,
			OutboundDestination: "DEST", SwapMode: protocol.SwapModeSequential,
		}
	}
	for _, tc := range []struct {
		own, partner, want string
	}{
		{"SEQ-A", "SEQ-B", "pickup@SEQ-A dropoff@DEST pickup@MARKET wait@- dropoff@SEQ-A"},
		{"SEQ-B", "SEQ-A", "pickup@SEQ-B dropoff@DEST pickup@MARKET wait@- dropoff@SEQ-B"},
	} {
		d := buildSequentialPerPositionEvacuate(claimAt(tc.own, tc.partner), to, "SEQ-A")
		if got := stepTrace(d.StepsA); got != tc.want {
			t.Errorf("%s steps =\n  %s\nwant\n  %s", tc.own, got, tc.want)
		}
		if d.StepsB != nil {
			t.Errorf("%s emitted a second step-list; per-node evacuate is one order per position", tc.own)
		}
	}
}
