package dispatch

import (
	"testing"

	"shingo/protocol"
)

// lane_gate_plan_pure_test.go — the invariant the gate-wait fence rests on.
//
// Brief 4 step 0 asked whether any order carries both a GATE wait (a precondition
// internal to Core, advanced only by the evaluator) and a STATION wait (a physical
// precondition at a station, reported by an OrderRelease). The answer is no: waits
// are minted in exactly two places and they are disjoint by order class — the two
// gated-plan builders below mint gate waits on plain orders only, and the
// Edge-authored complex plan mints station waits on coordinated orders only.
//
// So the fence can key on IsGateStaged rather than on a per-step marker. But
// IsGateStaged reads `WaitIndex == 0`, which names only the FIRST wait, and that is
// exact ONLY because a gated plan holds exactly one. That property is true and was
// unenforced — nothing broke if a builder grew a second wait; IsGateStaged would
// quietly stop covering the tail of the plan.
//
// This is the cheap half of what a per-step marker would have bought, without the
// part that lies: a marker would have labelled a plan `owner:"gate"` even when the
// plan was a gated rewrite that had just destroyed an Edge-authored one.

// TestGatedPlans_HoldExactlyOneWait pins the one-wait shape at both builders, and
// pins it through the CONSUMER as well as by counting — splitSegment is what turns
// a plan into appends, so "no second wait" is only meaningful as "splitSegment has
// nothing to hand out at index 1".
//
// MUTATION (verified): add a second `{Action: protocol.ActionWait, Node: gatePoint}`
// to either builder. The count assertion fires for that builder, and so does the
// moreWaits assertion — the first append would stop sealing the order, which is the
// concrete way a second wait would break the fence rather than merely widen it.
func TestGatedPlans_HoldExactlyOneWait(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		plan []resolvedStep
	}{
		{"store", buildGatedTransportPlan("SRC", "GATE", "DEST", false)},
		{"retrieve", buildGatedRetrievePlan("GATE", "SLOT", "LINE", false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			waits := 0
			for _, s := range tc.plan {
				if s.Action == protocol.ActionWait {
					waits++
				}
			}
			if waits != 1 {
				t.Fatalf("gated %s plan holds %d waits, want exactly 1 (%+v). IsGateStaged keys on "+
					"WaitIndex == 0, which names only the FIRST wait — a second one is a gate wait the "+
					"fence cannot see, and it would be releasable by a station", tc.name, waits, tc.plan)
			}

			// The first append must SEAL: moreWaits=false is what makes wait_index 1
			// mean "this order has no gate wait left", which is the half IsGateStaged
			// relies on to be exact rather than approximate.
			seg, moreWaits, _ := splitSegment(tc.plan, 0)
			if seg == nil {
				t.Fatalf("gated %s plan yields no segment at wait_index 0 — the valve would have "+
					"nothing to append", tc.name)
			}
			if moreWaits {
				t.Errorf("gated %s plan reports more waits after the first segment; the append would "+
					"leave the order unsealed and still gate-staged", tc.name)
			}

			// And nothing at index 1, which is the same statement from the other side.
			if seg, _, _ := splitSegment(tc.plan, 1); seg != nil {
				t.Errorf("gated %s plan yields a segment at wait_index 1 (%+v) — a second wait exists "+
					"and IsGateStaged cannot name it", tc.name, seg)
			}
		})
	}
}
