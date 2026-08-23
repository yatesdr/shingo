package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// stagedPressDiff builds one press-index diff in the staged tooling mode:
// seats marked on the OUTGOING claim, which is what puts the cell into the
// mode at all.
func stagedPressDiff(node string, seats []string, toStaging string) ChangeoverNodeDiff {
	from := &processes.NodeClaim{
		CoreNodeName:         node,
		SwapMode:             protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:          "PART-A",
		PairedCoreNode:       node + "_B",
		SecondPairedCoreNode: node + "_C",
		OutboundDestination:  "MARKET",
		ChangeoverEvacSeats:  seats,
	}
	to := &processes.NodeClaim{
		CoreNodeName:         node,
		SwapMode:             protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:          "PART-A",
		PairedCoreNode:       node + "_B",
		SecondPairedCoreNode: node + "_C",
		OutboundDestination:  "MARKET",
		InboundSource:        "EMPTIES",
		InboundStaging:       toStaging,
	}
	return ChangeoverNodeDiff{CoreNodeName: node, Situation: SituationEvacuate, FromClaim: from, ToClaim: to}
}

// The staged mode parks incoming bins at InboundStaging. Arming without one
// produces a plan whose supply legs have nowhere to go — discovered as robots
// idling mid-changeover — so it is refused at arm time.
func TestRefuseStagedChangeoverWithoutStaging(t *testing.T) {
	t.Parallel()
	err := refuseStagedChangeoverWithoutStaging([]ChangeoverNodeDiff{
		stagedPressDiff("PLN_002", []string{domain.EvacSeatFront}, ""),
	})
	if err == nil {
		t.Fatal("want a refusal when a staged-tooling cell has no inbound staging")
	}
	// NAMED FIELD AND NAMED CELL. "changeover requires inbound staging" sends
	// an engineer to the wrong page on a line with six presses.
	if !strings.Contains(err.Error(), "PLN_002") {
		t.Errorf("refusal must name the cell; got %q", err)
	}
	if !strings.Contains(err.Error(), "Inbound Staging") {
		t.Errorf("refusal must name the missing field; got %q", err)
	}
}

func TestRefuseStagedChangeoverWithoutStaging_NamesEveryCell(t *testing.T) {
	t.Parallel()
	err := refuseStagedChangeoverWithoutStaging([]ChangeoverNodeDiff{
		stagedPressDiff("PLN_002", []string{domain.EvacSeatFront}, ""),
		stagedPressDiff("PLN_004", []string{domain.EvacSeatPaired}, ""),
	})
	if err == nil {
		t.Fatal("want a refusal")
	}
	for _, want := range []string{"PLN_002", "PLN_004"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name every offending cell; %q missing from %q", want, err)
		}
	}
}

// SCOPED TO THIS MODE. Plain press-index production neither uses nor requires
// staging, and this gate must not become a reason a working line cannot change
// over. These are the rows that keep it honest.
func TestRefuseStagedChangeoverWithoutStaging_LeavesEverythingElseAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		diff ChangeoverNodeDiff
	}{
		{"press-index with NO seats marked", stagedPressDiff("PLN_002", nil, "")},
		{"staged mode WITH staging configured",
			stagedPressDiff("PLN_002", []string{domain.EvacSeatFront}, "IN-STAGE")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := refuseStagedChangeoverWithoutStaging([]ChangeoverNodeDiff{tc.diff}); err != nil {
				t.Errorf("must not refuse: %v", err)
			}
		})
	}

	// A non-press-index mode cannot be in the staged tooling mode at all, even
	// if a stale seat selection survives on its row.
	d := stagedPressDiff("ALN_001", []string{domain.EvacSeatFront}, "")
	d.FromClaim.SwapMode = protocol.SwapModeTwoRobot
	if err := refuseStagedChangeoverWithoutStaging([]ChangeoverNodeDiff{d}); err != nil {
		t.Errorf("a non-press-index claim is never in the staged mode: %v", err)
	}

	// A drop has no incoming claim; there is nothing to stage and nothing to
	// refuse.
	d2 := stagedPressDiff("PLN_002", []string{domain.EvacSeatFront}, "")
	d2.ToClaim = nil
	if err := refuseStagedChangeoverWithoutStaging([]ChangeoverNodeDiff{d2}); err != nil {
		t.Errorf("a drop has no incoming claim to stage: %v", err)
	}
}
