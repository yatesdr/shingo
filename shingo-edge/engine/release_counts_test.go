package engine

// release_counts_test.go — the cost instrument for the release layer.
//
// Not a budget pin. It measures what one act costs — SQLite reads and writes on
// the Edge's single connection, direct WarLink reads (3 s each at worst), and
// Core HTTP round trips — on the four paths the release work is held to: the
// pair click, the per-order click, the changeover node click, and the /view
// build. The numbers go into each step's report and are compared there; a later
// step pins /view once it is rebuilt (S6). Run with RELEASE_COUNTS=1.

import (
	"context"
	"os"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

func TestReleaseCosts(t *testing.T) {
	if os.Getenv("RELEASE_COUNTS") == "" {
		t.Skip("measurement instrument; run with RELEASE_COUNTS=1")
	}
	S := protocol.StatusStaged
	type path struct {
		name  string
		build func(h *relHarness)
		act   func(h *relHarness) error
	}
	view := func(h *relHarness) error {
		_, err := h.eng.stationService.BuildView(context.Background(), h.stationID)
		return err
	}
	withBin := func(b func(h *relHarness)) func(h *relHarness) {
		return func(h *relHarness) {
			b(h)
			h.core.binAt = &NodeBinInfo{NodeName: fxPress, BinID: fxBin, PayloadCode: fxPart, UOPRemaining: fxCount, Occupied: true}
		}
	}
	paths := []path{
		{"pair click / two_robot, curtain armed",
			withCurtain(curtainSafe, pairAt(pairSpec{mode: protocol.SwapModeTwoRobot}, "evac", S, "supply", S)), pairClick(dispEmpty)},
		{"pair click / press-index 2-pos, curtain armed",
			withCurtain(curtainSafe, pairAt(pairSpec{mode: protocol.SwapModeTwoRobotPressIndex}, "evac", S, "supply", S)), pairClick(dispEmpty)},
		{"pair click / two_robot consume, PULL PARTS (BinAtLineside)",
			withBin(pairAt(pairSpec{mode: protocol.SwapModeTwoRobot, role: protocol.ClaimRoleConsume}, "evac", S, "supply", S)), pairClick(dispPull(5))},
		{"per-order click / two_robot consume evac, PULL PARTS (BinAtLineside)",
			withBin(pairAt(pairSpec{mode: protocol.SwapModeTwoRobot, role: protocol.ClaimRoleConsume}, "evac", S, "supply", S)), orderClick("evac", dispPull(5))},
		{"per-order click / two_robot produce evac, curtain armed",
			withCurtain(curtainSafe, pairAt(pairSpec{mode: protocol.SwapModeTwoRobot}, "evac", S, "supply", S)), orderClick("evac", dispEmpty)},
		{"changeover node click / press-index tooling, curtain armed",
			withCurtain(curtainSafe, coAt(coSpec{mode: protocol.SwapModeTwoRobotPressIndex, tooling: true}, "evac", S, "supply", S)), nodeCOClick(dispNone)},
		{"request / two_robot produce",
			func(h *relHarness) { h.seedNode(pairSpec{mode: protocol.SwapModeTwoRobot}) },
			func(h *relHarness) error { _, err := h.eng.RequestProduceSwap(h.nodeID); return err }},
		{"request / single_robot produce",
			func(h *relHarness) { h.seedNode(pairSpec{mode: protocol.SwapModeSingleRobot}) },
			func(h *relHarness) error { _, err := h.eng.RequestProduceSwap(h.nodeID); return err }},
		{"per-order click / single_robot produce, curtain armed",
			withCurtain(curtainSafe, func(h *relHarness) { h.singleRobot(protocol.ClaimRoleProduce); statuses(h, "swap", S) }),
			orderClick("swap", dispEmpty)},
		{"per-order click / sequential produce (flip)",
			func(h *relHarness) { h.sequentialAB(protocol.ClaimRoleProduce, true); statuses(h, "removal", S) },
			orderClick("removal", dispEmpty)},
		{"/view / one paired tile, both staged, curtain armed",
			withCurtain(curtainSafe, pairAt(pairSpec{mode: protocol.SwapModeTwoRobot}, "evac", S, "supply", S)), view},
		{"/view / press-index tile in a changeover",
			withCurtain(curtainSafe, coAt(coSpec{mode: protocol.SwapModeTwoRobotPressIndex, tooling: true}, "evac", S, "supply", S)), view},
	}
	for _, p := range paths {
		h := newRelHarness(t)
		p.build(h)
		h.mark()
		testutil.MustNoErr(t, p.act(h), p.name)
		t.Logf("COUNT %-70s %s", p.name, h.counts())
	}
}
