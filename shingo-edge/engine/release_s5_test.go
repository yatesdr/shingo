package engine

import (
	"errors"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingoedge/release"
)

// release_s5_test.go — S5's pins: the intent, its wakes, and G7 over Core's
// point. Same runner and outcome as the matrix (release_matrix_test.go).

// intentOf renders a leg's release intent for the outcome: "-" for none,
// "held@N" for one not yet sent, "sent@N" once its release went out.
func intentOf(h *relHarness, leg string) string {
	in, err := release.DecodeIntent(h.order(leg).ReleaseIntent)
	switch {
	case err != nil:
		return "unreadable"
	case in == nil:
		return "-"
	case in.Sent():
		return fmt.Sprintf("sent@%d", in.StationWait)
	}
	return fmt.Sprintf("held@%d", in.StationWait)
}

func pIntents(h *relHarness) []string {
	var out []string
	for _, l := range h.legs {
		out = append(out, "intent:"+l.name+"="+intentOf(h, l.name))
	}
	return out
}

// lifts reports Core's BinPickedUp for a leg lifting at node (anywhere: the
// wake comes before the handler's station-node filter).
func lifts(leg, node string) func(h *relHarness) error {
	return func(h *relHarness) error {
		h.lifted[node] = true
		h.eng.HandleBinPickedUp(h.order(leg).UUID, fxBin, node)
		return nil
	}
}

// floor runs the intent worker's 15 s floor once.
func floor(h *relHarness) error { h.eng.wakeIntentNodes("floor"); return nil }

// coreBack makes Core reachable again for releasePoints.
func coreBack(h *relHarness) error {
	h.core.mu.Lock()
	h.core.pointsDown = false
	h.core.mu.Unlock()
	return nil
}

// stagesAtLane reports Core's OrderStaged at a lane wait (no station
// ordinal): the lane's own coordination, which no station button releases.
func stagesAtLane(leg string) func(h *relHarness) error {
	return func(h *relHarness) error {
		h.handler.HandleOrderStaged(&protocol.Envelope{}, &protocol.OrderStaged{OrderUUID: h.order(leg).UUID,
			Detail: "stub: lane wait", WaitKind: protocol.WaitKindLane})
		return nil
	}
}

// liftsElsewhere is a lift Core's point records at node while the BinPickedUp
// names a location the pickup handler's station filter ignores: the intent
// wake must not depend on that filter.
func liftsElsewhere(leg, node string) func(h *relHarness) error {
	return func(h *relHarness) error {
		h.lifted[node] = true
		h.eng.HandleBinPickedUp(h.order(leg).UUID, fxBin, "SYN-AISLE-9")
		return nil
	}
}

// held runs a click that must hold (not refuse, not fail) part-way through a
// cell, and records the hold's gate in the outcome.
func held(act func(h *relHarness) error) func(h *relHarness) error {
	return func(h *relHarness) error {
		err := act(h)
		var he *release.HeldError
		if !errors.As(err, &he) {
			return fmt.Errorf("want the click held, got %v", err)
		}
		h.extra = append(h.extra, "click=held:"+he.Gate)
		return nil
	}
}

func TestReleaseS5Pins(t *testing.T) {
	t.Parallel()
	twoRobot := pairSpec{mode: protocol.SwapModeTwoRobot}
	twoRobotConsume := pairSpec{mode: protocol.SwapModeTwoRobot, role: protocol.ClaimRoleConsume}
	pi2 := pairSpec{mode: protocol.SwapModeTwoRobotPressIndex}
	S, D, T := protocol.StatusStaged, protocol.StatusDispatched, protocol.StatusInTransit
	runRelCells(t, []relCell{
		// The pair click remembers a supply not yet at its wait (G1); an Edge
		// restart keeps it (the intent is on the row); the supply stages, then
		// waits for the evac's lift (G7) and goes at it.
		{name: "S5/a held supply survives a restart and goes at the evac's lift",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | intent:evac=sent@0 | intent:supply=sent@0",
			build: pairAt(twoRobot, "evac", S, "supply", D),
			act: seq(pairClick(dispEmpty), func(h *relHarness) error { h.restart(); return nil },
				stagesAt("supply", 0), lifts("evac", fxPress)),
			probe: pIntents},
		// N-b: R2 staged, R1 still driving to its wait. Both hold (R2 waits on
		// R1's lift, R1's co-release partner is not going); they go together
		// when R1 parks.
		{name: "S5/press-index unflipped, R1 driving: both hold, then go together when R1 parks",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | click=held:G7 | intent:evac=sent@0 | intent:supply=sent@0",
			build: pairAt(pi2, "evac", T, "supply", S),
			act:   seq(held(pairClick(dispEmpty)), stagesAt("evac", 0)),
			probe: pIntents},
		// A Core rejection clears the intent in its own statement before the
		// rollback's emit: the leg does not re-fire by itself.
		{name: "S5/a rejected release clears its intent: no re-fire",
			want:  "ok | evac=staged supply=in_transit | rel=evac,supply | ingest=0 capred=0 | intent:evac=- | intent:supply=sent@0",
			build: pairAt(twoRobotConsume, "evac", S, "supply", S),
			act:   seq(pairClick(dispEmpty), refuse("evac", "invalid_state"), stagesAt("evac", 0)),
			probe: pIntents},
		// Core's fleet refused the appended segment and re-staged the leg at
		// the same wait after its release went out (the faulted no-op): the
		// release is re-sent once, and only once.
		{name: "S5/staged again at the same wait: re-sent once",
			want:  "ok | evac=staged supply=in_transit | rel=evac,supply,evac | ingest=1 capred=0 | intent:evac=sent@0 | intent:supply=sent@0",
			build: pairAt(twoRobot, "evac", S, "supply", S),
			act: seq(pairClick(dispEmpty),
				func(h *relHarness) error { h.setStatus("evac", S); return nil }, stagesAt("evac", 0),
				func(h *relHarness) error { h.setStatus("evac", S); return nil }, stagesAt("evac", 0)),
			probe: pIntents},
		// G3: Core unreachable holds; the floor fires it when Core answers.
		{name: "S5/Core unreachable: held, then the floor releases when Core is back",
			want: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | click=held:G3 | intent:evac=sent@0 | intent:supply=sent@0",
			build: func(h *relHarness) {
				pairAt(twoRobot, "evac", S, "supply", S)(h)
				h.core.mu.Lock()
				h.core.pointsDown = true
				h.core.mu.Unlock()
			},
			act:   seq(held(pairClick(dispEmpty)), coreBack, floor),
			probe: pIntents},
		// The lift is the wake wherever it is reported.
		{name: "S5/a held supply goes at a lift reported away from the station",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | intent:evac=sent@0 | intent:supply=sent@0",
			build: pairAt(twoRobot, "evac", S, "supply", D),
			act:   seq(pairClick(dispEmpty), stagesAt("supply", 0), liftsElsewhere("evac", fxPress)),
			probe: pIntents},
		// G2: a leg parked at a lane wait is the lane's, not the button's.
		{name: "S5/a leg at a lane wait is out of the act's scope",
			gate:  "G2",
			want:  "hold:lift | evac=staged supply=staged | rel=- | ingest=1 capred=0 | intent:evac=- | intent:supply=held@0",
			build: pairAt(twoRobot, "evac", S, "supply", S),
			act:   seq(stagesAtLane("evac"), pairClick(dispEmpty)),
			probe: pIntents},
		// The per-order click names one order: at a lane wait it refuses in
		// Core's words instead of reporting a release that sent nothing.
		{name: "S5/per-order click on a leg at a lane wait: refused",
			gate:  "G2",
			want:  "err:order 2 is waiting on a lane, not on the station | evac=staged supply=staged | rel=- | ingest=0 capred=0 | intent:evac=- | intent:supply=-",
			build: pairAt(twoRobot, "evac", S, "supply", S),
			act:   seq(stagesAtLane("evac"), orderClick("evac", dispEmpty)),
			probe: pIntents},
		// The sibling ending is a wake: with the evac gone there is nothing
		// left for the held supply to wait on.
		{name: "S5/a held supply goes when its evac ends",
			want:  "ok | evac=confirmed supply=in_transit | rel=evac,supply | ingest=1 capred=0 | intent:evac=- | intent:supply=sent@0",
			build: pairAt(twoRobot, "evac", S, "supply", D),
			act:   seq(pairClick(dispEmpty), stagesAt("supply", 0), confirms("evac")),
			probe: pIntents},
	})
}
