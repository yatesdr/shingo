package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// ---------------------------------------------------------------------------
// N1-c — THE REFILL IS FOR THE INCOMING STYLE, AND THE LEG NOW SAYS SO.
//
// A changeover swap is one order doing two jobs. It carries the FROM-style
// payload because its opening pickup has to find the bin physically on the
// line; the carrier it fetches on the same trip has to suit the style arriving.
// `Empty` was believed to reconcile those — "marking it Empty also drops that
// leg's payload filter" — and it does drop the full-bin content match, but not
// bin-type compatibility, which Core resolves against whatever payload reaches
// PayloadBinTypeAdvisoryClause.
//
// So the press was handed a carrier of the type it was LEAVING. The sim caught
// it in both directions on 2026-08-24: PANEL-A → PANEL-C delivered a STANDARD
// carrier to a press now riding STANDARD-SM with an SM empty sitting unchosen in
// the same group, and PANEL-C → PANEL-A parked both supply legs on "waiting for
// an empty bin" — waiting for an SM carrier, for a press switching to STANDARD,
// while every SM bin in the plant was full. That changeover could not complete
// until an operator abandoned the legs.
// ---------------------------------------------------------------------------

// inboundPickup returns the Empty pickup at src, the leg that fetches the
// replacement carrier.
func inboundPickup(t *testing.T, steps []protocol.ComplexOrderStep, src string) protocol.ComplexOrderStep {
	t.Helper()
	for _, s := range steps {
		if s.Action == protocol.ActionPickup && s.Node == src && s.Empty {
			return s
		}
	}
	t.Fatalf("no empty pickup at %s in %v", src, steps)
	return protocol.ComplexOrderStep{}
}

// TestPerPositionSwapRefillNamesTheIncomingPayload is the plain path — no marks,
// no tooling. It is where the defect lives and where it has always lived.
func TestPerPositionSwapRefillNamesTheIncomingPayload(t *testing.T) {
	t.Parallel()
	from := pressClaim("PRESS", "PRESS_B", "PART-A")
	to := pressClaim("PRESS", "PRESS_B", "PART-C")

	disp := buildPressIndexPerPositionSwap(&from, &to)
	pickup := inboundPickup(t, disp.StepsA, "EMPTIES")
	if pickup.PayloadCode != "PART-C" {
		t.Errorf("refill leg resolves against %q, want the INCOMING PART-C.\n"+
			"The order carries PART-A because its opening pickup must find the bin on the line. "+
			"If the refill cannot name its own payload, Core picks a carrier compatible with "+
			"PART-A — the type the press is leaving.", pickup.PayloadCode)
	}
	// The order itself still carries the from-style payload. That is not a
	// wart to clean up later: the opening pickup depends on it.
	if !disp.CarriesFromPayloadA {
		t.Error("the order stopped carrying the from-style payload — its first pickup will now " +
			"filter for the incoming payload and find nothing at the position")
	}
}

// TestToolingPositionRefillNamesTheIncomingPayload is the same contract on the leg
// this round built: a marked position's clearance-and-refill.
func TestToolingPositionRefillNamesTheIncomingPayload(t *testing.T) {
	t.Parallel()
	from := pressClaim("PRESS", "PRESS_B", "PART-A")
	from.ChangeoverEvacNodes = []string{"PRESS", "PRESS_B"}
	to := pressClaim("PRESS", "PRESS_B", "PART-C")
	to.InboundStaging = "IN-STAGE"

	tc := planToolingChangeover([]processes.NodeClaim{from}, []processes.NodeClaim{to})
	if len(tc.presses) != 1 {
		t.Fatalf("expected one marked press, got %d", len(tc.presses))
	}
	node := &processes.Node{ID: 1, CoreNodeName: "PRESS", Name: "PRESS"}
	action := toolingPositionAction(tc.presses[0], "PRESS", node)
	if action.SupplyOrder == nil || action.SupplyOrder.Complex == nil {
		t.Fatal("position action has no complex supply order")
	}
	pickup := inboundPickup(t, action.SupplyOrder.Complex.Steps, "EMPTIES")
	if pickup.PayloadCode != "PART-C" {
		t.Errorf("marked position's refill resolves against %q, want the INCOMING PART-C.\n"+
			"A tool change is the moment the carrier type most often changes — it is the "+
			"reason the change is happening.", pickup.PayloadCode)
	}
	if action.SupplyOrder.Complex.PayloadCode != "PART-A" {
		t.Errorf("position order carries %q, want the FROM-style PART-A for its opening pickup",
			action.SupplyOrder.Complex.PayloadCode)
	}
}

// A same-payload changeover must send nothing extra on the wire: the field is
// for legs that need to disagree with their order, and here they agree.
func TestRefillNamesNothingWhenThePayloadIsUnchanged(t *testing.T) {
	t.Parallel()
	from := pressClaim("PRESS", "PRESS_B", "PART-A")
	to := pressClaim("PRESS", "PRESS_B", "PART-A")

	disp := buildPressIndexPerPositionSwap(&from, &to)
	pickup := inboundPickup(t, disp.StepsA, "EMPTIES")
	if pickup.PayloadCode != "" {
		t.Errorf("refill named %q on a changeover that does not change payload.\n"+
			"Say nothing and the order's payload applies, which is the same answer — and the "+
			"wire stays exactly as it was for every plant that never hits this case.",
			pickup.PayloadCode)
	}
}
