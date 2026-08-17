package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOccupancyIsTakenOnlyAtTheKnownSeams is the drift guard for the rule that
// PLAN §R.54 cost 997 seconds of frozen plant to learn.
//
// THE RULE: Hold B records where a robot IS, so a dispatch takes rows for the
// lanes it ENTERS — which is not the same as the lanes its plan names. A create
// bound for a gated lane stops at the mark; that lane's row belongs to the tail
// append (appendGateTail). Every dispatch seam has to answer "am I the moment the
// robot goes in", and the two answer it in their own vocabulary: the gated create
// walks its spliced plan (planNodes(preWait)), the compound leg asks the lane
// (enteredAtDispatch), because it takes before its plan is spliced.
//
// SO THE GUARD IS ON THE SEAM LIST, NOT ON THE RULE. Enforcing the rule inside
// TakeLaneOccupancy was tried and is wrong — it is also the general "this order IS
// in this lane" primitive, and a gated lane holds rows like any other, which is
// the entire mechanism of one-robot-at-a-time. Six existing tests said so at once.
// What can be guarded is that a THIRD dispatch seam cannot appear without someone
// coming here and reading the rule, which is exactly how the compound leg's
// version went unnoticed for as long as it did: it was written before the gate
// existed and nothing ever re-asked it.
//
// A SOURCE TEST because that is where the claim lives. "Presence is written in
// these places and no others" is a statement about the code's shape, and no
// fixture can prove a call site does not exist somewhere else. The behavioural
// half — that the compound seam takes no row on a lane it stands outside of — is
// TestGatedLeg_TakesNoOccupancyOnTheLaneItStandsOutsideOf.
//
// Test files are exempt: a fixture stating that a robot is inside a lane is
// declaring a premise, not dispatching, and several legitimately do.
func TestOccupancyIsTakenOnlyAtTheKnownSeams(t *testing.T) {
	// The doors themselves, and the seams allowed to open them.
	writers := map[string]string{
		"lane_gate.go":          "the two doors: TakeLaneOccupancy (by node) and takeLaneOccupancyByID (the gated append's)",
		"fleet_handover.go":     "commitToFleet — every create seam funnels here, and its callers pass what they enter",
		"compound.go":           "the compound leg, which filters through enteredAtDispatch (PLAN §R.54)",
		"lane_gate_dispatch.go": "the gated append, which is the moment a staged robot actually enters",
		// THE FOURTH SEAM, AND IT ANSWERED THE RULE'S QUESTION BEFORE IT WAS ADDED
		// HERE. A destination-deferred dig leg enters its DUG lane at dispatch
		// (compound.go's seam, above) and its DESTINATION lane at release, because
		// that is when the destination exists at all — the choice is what the dwell
		// defers. So the row this file takes is the destination's, at the moment the
		// tail is appended, which is the same moment lane_gate_dispatch.go takes the
		// inbound one and for the same reason.
		"dig_dwell.go": "the release-time resolver, which enters the destination lane it has just chosen",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the dispatch package: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if _, known := writers[name]; known {
			continue
		}
		src, rErr := os.ReadFile(filepath.Clean(name))
		if rErr != nil {
			t.Fatalf("read %s: %v", name, rErr)
		}
		for _, call := range []string{"AcquireOccupancy(", "TakeLaneOccupancy(", "takeLaneOccupancyByID("} {
			if strings.Contains(string(src), call) {
				t.Errorf("%s writes lane occupancy (%s), and it is not one of the known seams.\n"+
					"Hold B records where a robot IS. A new seam has to decide whether ITS dispatch is "+
					"the moment the robot enters — a create bound for a GATED lane stops at the mark and "+
					"its row belongs to the tail append, so passing the plan's endpoints raw declares a "+
					"robot inside a corridor it is standing next to. That phantom row refused the only "+
					"order able to break a four-order cycle on the rig, frozen 997 seconds (PLAN §R.54). "+
					"Pass what you ENTER: planNodes(preWait) if you have a spliced plan, "+
					"enteredAtDispatch if you do not — then add this file here.", name, call)
				break
			}
		}
	}
}
