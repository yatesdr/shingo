package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
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

func seatClaims() (*processes.NodeClaim, *processes.NodeClaim) {
	from := &processes.NodeClaim{
		CoreNodeName:              "PRESS_B",
		Role:                      protocol.ClaimRoleProduce,
		SwapMode:                  domain.SwapModePressPosition,
		PayloadCode:               "PART-A",
		OutboundDestination:       "MARKET",
		ChangeoverEvacDestination: "TOOLING-BAY",
	}
	to := &processes.NodeClaim{
		CoreNodeName:   "PRESS_B",
		Role:           protocol.ClaimRoleProduce,
		SwapMode:       domain.SwapModePressPosition,
		PayloadCode:    "PART-B",
		InboundSource:  "EMPTIES",
		InboundStaging: "IN-STAGE",
	}
	return from, to
}

// The staged shape: lift the blocking bin off the seat, get it off the line,
// fetch the replacement, PARK AT STAGING holding it, and deliver on release.
func TestBuildPressIndexSeatEvacuate_Shape(t *testing.T) {
	t.Parallel()
	from, to := seatClaims()
	d := buildPressIndexSeatEvacuate(from, to)

	want := "pickup@PRESS_B dropoff@TOOLING-BAY pickup@EMPTIES wait@IN-STAGE dropoff@PRESS_B"
	if got := stepTrace(d.StepsA); got != want {
		t.Errorf("steps =\n  %s\nwant\n  %s", got, want)
	}
	if d.StepsB != nil {
		t.Error("a seat is one robot; the fan-out already split the cell")
	}
	// The order opens by lifting the OLD bin, so it must carry the from-style
	// payload or the pickup filters for the new one and finds no bin (ALN_001).
	if !d.CarriesFromPayloadA {
		t.Error("CarriesFromPayloadA = false; the opening pickup lifts an old-style bin")
	}

	// THE WAIT IS AT THE STAGING NODE, not bare. That is what gets the robot
	// out of the press cell while it holds the incoming bin — a tooling change
	// can take a shift, and a robot parked on the apron blocks the millwrights.
	var waits int
	for _, s := range d.StepsA {
		if s.Action == protocol.ActionWait {
			waits++
			if s.Node != "IN-STAGE" {
				t.Errorf("wait node = %q, want IN-STAGE", s.Node)
			}
			if s.WaitKind != waitKindStation {
				t.Errorf("wait kind = %q, want %q", s.WaitKind, waitKindStation)
			}
		}
	}
	if waits != 1 {
		t.Errorf("waits = %d, want exactly 1 (the tooling-done gate)", waits)
	}
}

// Blank ChangeoverEvacDestination falls back to OutboundDestination — the
// whole compatibility story for the field, asserted where it is consumed.
func TestBuildPressIndexSeatEvacuate_DestinationFallback(t *testing.T) {
	t.Parallel()
	from, to := seatClaims()
	from.ChangeoverEvacDestination = ""
	d := buildPressIndexSeatEvacuate(from, to)
	if !strings.Contains(stepTrace(d.StepsA), "dropoff@MARKET") {
		t.Errorf("blank evac destination must fall back to OutboundDestination; got %s", stepTrace(d.StepsA))
	}
}

// A produce seat's replacement is a fresh EMPTY carrier. Without the flag the
// dispatch hunts a full payload-matched bin in the empties pool and fails.
func TestBuildPressIndexSeatEvacuate_ProduceFetchesAnEmpty(t *testing.T) {
	t.Parallel()
	from, to := seatClaims()
	d := buildPressIndexSeatEvacuate(from, to)
	var found bool
	for _, s := range d.StepsA {
		if s.Action == protocol.ActionPickup && s.Node == "EMPTIES" {
			found = true
			if !s.Empty {
				t.Error("the InboundSource pickup on a produce seat must be flagged Empty")
			}
		}
	}
	if !found {
		t.Fatal("no pickup at the inbound source")
	}

	// A consume seat pulls a full bin, so the flag must NOT be set — otherwise
	// it fetches an empty carrier onto a line that needs parts.
	to.Role = protocol.ClaimRoleConsume
	for _, s := range buildPressIndexSeatEvacuate(from, to).StepsA {
		if s.Action == protocol.ActionPickup && s.Node == "EMPTIES" && s.Empty {
			t.Error("a consume seat must fetch a FULL bin, not an empty carrier")
		}
	}
}

// An empty dispatch is the planner's "I rejected this" signal.
func TestBuildPressIndexSeatEvacuate_RejectsMissingRouting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		mut  func(f, t *processes.NodeClaim)
	}{
		{"no evac destination and no outbound", func(f, _ *processes.NodeClaim) {
			f.ChangeoverEvacDestination, f.OutboundDestination = "", ""
		}},
		{"no inbound source", func(_, t *processes.NodeClaim) { t.InboundSource = "" }},
		{"no staging node", func(_, t *processes.NodeClaim) { t.InboundStaging = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			from, to := seatClaims()
			tc.mut(from, to)
			if d := buildPressIndexSeatEvacuate(from, to); !d.rejected() {
				t.Errorf("want a rejected dispatch; got %s", stepTrace(d.StepsA))
			}
		})
	}
}

// SEQUENTIAL IS UNCHANGED BY THE EXTRACTION. Both builders now share
// buildToolingEvacSteps, and sequential's bare wait is its own deliberate
// choice — a regression here would be a shared helper quietly changing a mode
// nobody was working on.
func TestBuildSequentialChangeoverEvacuate_StillCarriesAcrossABareWait(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName: "SEQ-A", PairedCoreNode: "SEQ-B",
		OutboundDestination: "DEST", SwapMode: protocol.SwapModeSequential,
	}
	// InboundStaging IS SET, and unused by this builder. That is the point:
	// with it blank a staged wait leaking in through the shared helper would
	// still render as a bare wait and this test could not tell.
	to := &processes.NodeClaim{CoreNodeName: "SEQ-A", InboundSource: "MARKET", InboundStaging: "SEQ-STAGE"}
	d := buildSequentialChangeoverEvacuate(from, to)

	wantA := "pickup@SEQ-A dropoff@DEST pickup@MARKET wait@- dropoff@SEQ-A"
	wantB := "pickup@SEQ-B dropoff@DEST pickup@MARKET wait@- dropoff@SEQ-B"
	if got := stepTrace(d.StepsA); got != wantA {
		t.Errorf("A steps =\n  %s\nwant\n  %s", got, wantA)
	}
	if got := stepTrace(d.StepsB); got != wantB {
		t.Errorf("B steps =\n  %s\nwant\n  %s", got, wantB)
	}
}
