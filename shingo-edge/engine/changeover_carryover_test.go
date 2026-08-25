package engine

import (
	"strings"
	"testing"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// ---------------------------------------------------------------------------
// CARRY-OVER PARTS — a marked position whose part is COMMON to both styles.
//
// The marked-position arm of DiffStyleClaims wins over the same-payload arm, which
// is right: a tool change needs the floor space whatever the part is. But it
// means a part the cell KEEPS gets sent away and an identical empty carrier
// fetched to replace it — the system round-trips a bin it is about to want
// back.
//
// Which answer is right is a property of the part and the cell, so it is
// configured, not derived: replace (today), keep_lineside (the bin does not
// move), outbound_staging (the same bin hops out and comes back on the
// tooling-done release). When the payloads DIFFER the question does not arise —
// the bin has to change anyway — and all three behave as replace.
// ---------------------------------------------------------------------------

// carryoverPress is a marked press-index claim pair. Same payload on both sides
// unless the caller changes one, so the default is a carry-over.
func carryoverPress(disp domain.CarryoverDisposition) (from, to processes.NodeClaim) {
	from = pressClaim("PRESS", "PRESS_B", "PART-A")
	from.ChangeoverEvacPositions = []string{domain.EvacPositionFront, domain.EvacPositionPaired}
	from.OutboundStaging = "OUT-STAGE"
	from.ChangeoverCarryoverDisposition = disp

	to = pressClaim("PRESS", "PRESS_B", "PART-A")
	to.InboundStaging = "IN-STAGE"
	return from, to
}

// CONTROL — replace is what shipped, and a column default of replace means
// every existing mark behaves exactly as it did before this option existed.
func TestCarryover_ReplaceKeepsTodaysShape(t *testing.T) {
	t.Parallel()
	from, to := carryoverPress(domain.CarryoverReplace)
	plan := toolingTestPlan(t, []processes.NodeClaim{from}, []processes.NodeClaim{to},
		map[string]string{"PART-A": "BIG"}, "PRESS", "PRESS_B")

	got := stepString(actionFor(t, plan, "PRESS").SupplyOrder)
	want := "pickup@PRESS,dropoff@MARKET,pickup@EMPTIES(empty),wait@IN-STAGE,dropoff@PRESS"
	if got != want {
		t.Errorf("replace changed the proven shape.\n got = %s\nwant = %s", got, want)
	}
}

// KEEP_LINESIDE — the bin does not move, so NOTHING is planned for the position.
// The part does not affect the tool change, which makes the position unmarked for
// THIS changeover; the diff engine already answers that correctly for a common
// part on an unmarked position, so the honest implementation is to let it.
func TestCarryover_KeepLinesideTouchesNothing(t *testing.T) {
	t.Parallel()
	from, to := carryoverPress(domain.CarryoverKeepLineside)
	plan := toolingTestPlan(t, []processes.NodeClaim{from}, []processes.NodeClaim{to},
		map[string]string{"PART-A": "BIG"}, "PRESS", "PRESS_B")

	for _, a := range plan.Actions {
		if a.SupplyOrder != nil || a.EvacOrder != nil {
			t.Errorf("keep_lineside planned an order at %s: %s\n"+
				"The bin stays on the position. A leg here is a robot moving a part the cell is "+
				"keeping, which is the waste this option exists to remove.",
				a.CoreNodeName, allSteps(a))
		}
	}
}

// OUTBOUND_STAGING — the SAME bin, out and back. Not a fresh carrier: the
// short hop clears the floor and the bin returns on the tooling-done release
// the inbound legs already hang off.
func TestCarryover_OutboundStagingWalksTheSameBinOutAndBack(t *testing.T) {
	t.Parallel()
	from, to := carryoverPress(domain.CarryoverOutboundStaging)
	plan := toolingTestPlan(t, []processes.NodeClaim{from}, []processes.NodeClaim{to},
		map[string]string{"PART-A": "BIG"}, "PRESS", "PRESS_B")

	got := stepString(actionFor(t, plan, "PRESS").SupplyOrder)
	want := "pickup@PRESS,dropoff@OUT-STAGE,wait@OUT-STAGE,pickup@OUT-STAGE,dropoff@PRESS"
	if got != want {
		t.Errorf("outbound_staging shape wrong.\n got = %s\nwant = %s", got, want)
	}
	// BIN IDENTITY, not just bin type: a leg that fetches from the empties pool
	// has replaced the part with an identical one and quietly defeated the
	// whole option.
	if strings.Contains(got, "EMPTIES") {
		t.Errorf("outbound_staging fetched a fresh carrier: %s\n"+
			"The bin that comes back must be the bin that left — that is the difference "+
			"between this and replace.", got)
	}
	// And it holds, on the same gate the inbound legs use, so one tooling-done
	// click brings everything in together.
	if !strings.Contains(got, "wait@OUT-STAGE") {
		t.Errorf("the returning bin does not wait for the setup to finish: %s", got)
	}
}

// A DIFFERENT PART IS NOT A CARRY-OVER. The disposition is never consulted:
// the bin has to leave whatever it says, and pretending otherwise would leave
// the wrong part on the press.
func TestCarryover_DifferingPartIgnoresTheDisposition(t *testing.T) {
	t.Parallel()
	for _, disp := range domain.CarryoverDispositions() {
		from, to := carryoverPress(disp)
		to.PayloadCode = "PART-C" // the style changes what the press makes

		plan := toolingTestPlan(t, []processes.NodeClaim{from}, []processes.NodeClaim{to},
			map[string]string{"PART-A": "BIG", "PART-C": "BIG"}, "PRESS", "PRESS_B")

		a := actionFor(t, plan, "PRESS")
		got := allSteps(a)
		if !strings.Contains(got, "pickup@PRESS") {
			t.Errorf("disposition %q left the outgoing bin on the position when the part CHANGED.\n"+
				"steps = %s\nA carry-over disposition is about a bin the cell keeps; there is "+
				"no such bin here.", disp, got)
		}
		if !strings.Contains(got, "pickup@EMPTIES") {
			t.Errorf("disposition %q fetched no replacement for a changed part.\nsteps = %s",
				disp, got)
		}
	}
}

// The disjoint shape is not a carry-over either, even when the payload matches:
// the position is being vacated, so there is nowhere to keep the bin and nothing to
// bring it back to.
func TestCarryover_DisjointPositionIsNotACarryover(t *testing.T) {
	t.Parallel()
	from := pressClaim("PLN_001", "PLN_002", "PART-A")
	from.ChangeoverEvacPositions = []string{domain.EvacPositionFront, domain.EvacPositionPaired}
	from.OutboundStaging = "OUT-STAGE"
	from.ChangeoverCarryoverDisposition = domain.CarryoverKeepLineside
	to := pressClaim("PLN_005", "PLN_006", "PART-A")
	to.InboundStaging = "IN-STAGE"

	plan := toolingTestPlan(t, []processes.NodeClaim{from}, []processes.NodeClaim{to},
		map[string]string{"PART-A": "BIG"}, "PLN_001", "PLN_002", "PLN_005", "PLN_006")

	a := actionFor(t, plan, "PLN_001")
	if got := allSteps(a); !strings.Contains(got, "pickup@PLN_001") {
		t.Errorf("a vacated position kept its bin because the payload matched.\nsteps = %s\n"+
			"The incoming style runs elsewhere: this position is being emptied, and keeping a bin "+
			"on it leaves material at a node no style claims.", got)
	}
}
