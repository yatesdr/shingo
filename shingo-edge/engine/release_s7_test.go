package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingoedge/release"
	storeorders "shingoedge/store/orders"
)

// release_s7_test.go — S7's pins: a creation that lifts a bin off a curtained
// node gets a station wait in front of the pickup and its intent at creation.
// A drop gets no wait: the curtain was cleared to let the robot in, and the
// robot finishes.

// backfillOf is the sequential Order B the removal's release created, or nil.
func backfillOf(h *relHarness) *storeorders.Order {
	h.t.Helper()
	a := h.order("removal")
	if a.SiblingOrderID == nil {
		return nil
	}
	b, err := h.db.GetOrder(*a.SiblingOrderID)
	if err != nil {
		return nil
	}
	return b
}

// pOrderWaits renders one order: its steps' waits, its intent and its chip.
func pOrderWaits(h *relHarness, label string, o *storeorders.Order) []string {
	if o == nil {
		return []string{label + "=none"}
	}
	in, _ := release.DecodeIntent(o.ReleaseIntent)
	intent := "-"
	switch {
	case in != nil && in.Sent():
		intent = fmt.Sprintf("sent@%d", in.StationWait)
	case in != nil:
		intent = fmt.Sprintf("held@%d system=%v", in.StationWait, in.System)
	}
	waits := 0
	steps, _ := h.db.GetOrderStepsJSON(o.ID)
	for _, s := range decodeStepsOrNil(steps) {
		if s.Action == protocol.ActionWait {
			waits++
		}
	}
	chip := o.ReleaseHeld
	if chip == "" {
		chip = "-"
	}
	return []string{fmt.Sprintf("%s: waits=%d intent=%s chip=%q", label, waits, intent, chip)}
}

// pBackfill renders Order B.
func pBackfill(h *relHarness) []string { return pOrderWaits(h, "B", backfillOf(h)) }

// pSupply renders the changeover's supply leg (the per-position swap).
func pSupply(h *relHarness) []string { return pOrderWaits(h, "supply", h.order("supply")) }

func decodeStepsOrNil(raw string) []protocol.ComplexOrderStep {
	s, err := decodeSteps(raw)
	if err != nil {
		return nil
	}
	return s
}

// supplyStages is Core parking the supply leg at its first station wait.
func supplyStages(h *relHarness) error {
	n := 0
	h.handler.HandleOrderStaged(&protocol.Envelope{}, &protocol.OrderStaged{OrderUUID: h.order("supply").UUID, Detail: "stub: parked",
		StationWait: &n, WaitKind: protocol.WaitKindStation})
	return nil
}

// namesB registers Order B as the outcome's leg "B".
func namesB(h *relHarness) error {
	b := backfillOf(h)
	if b == nil {
		return fmt.Errorf("no Order B")
	}
	h.addLeg("B", b.ID)
	return nil
}

// curtainReads sets the curtain tag's reading (the cell's bypass button).
func curtainReads(reading bool) func(h *relHarness) error {
	return func(h *relHarness) error { h.wl.set(reading); return nil }
}

// pressesSupply is an operator's RELEASE on the supply leg, which must hold.
func pressesSupply(h *relHarness) error {
	return held(func(h *relHarness) error { return h.eng.ReleaseOrderWithLineside(h.leg("supply"), dispNone) })(h)
}

// coPerPositionCurtained is a press-index per-position changeover whose
// position is curtained before the changeover creates its swap.
var coPerPositionCurtained = coSpec{mode: protocol.SwapModeTwoRobotPressIndex, perPosition: true, curtained: true}

func TestReleaseS7Pins(t *testing.T) {
	t.Parallel()
	runRelCells(t, []relCell{
		{name: "S7/sequential B only drops on a curtained line: no wait, no intent, it finishes",
			want:  "ok | removal=in_transit B=submitted | rel=removal | ingest=1 capred=0 | B: waits=0 intent=- chip=\"-\"",
			build: seqWithCurtain(curtainSafe),
			act:   seq(orderClick("removal", dispEmpty), namesB), probe: pBackfill},
		{name: "S7/sequential B with the curtain live: still no wait, it finishes",
			want:  "ok | removal=in_transit B=submitted | rel=removal | ingest=1 capred=0 | B: waits=0 intent=- chip=\"-\"",
			build: seqWithCurtain(curtainSafe),
			act:   seq(orderClick("removal", dispEmpty), namesB, curtainReads(curtainLive)), probe: pBackfill},
		{name: "S7/control: no curtain, no wait, no intent",
			want: "ok | removal=in_transit B=submitted | rel=removal | ingest=1 capred=0 | B: waits=0 intent=- chip=\"-\"",
			build: func(h *relHarness) {
				h.sequentialAB(protocol.ClaimRoleProduce, true)
				statuses(h, "removal", protocol.StatusStaged)
			},
			act: seq(orderClick("removal", dispEmpty), namesB), probe: pBackfill},
		{name: "S7/per-position swap at a curtained position: one wait, before the lift, the Edge's own intent",
			want:  "ok | supply=submitted | rel=- | ingest=0 capred=0 | supply: waits=1 intent=held@0 system=true chip=\"-\"",
			build: coAt(coPerPositionCurtained),
			act:   seq(), probe: pSupply},
		{name: "S7/the swap parks with the curtain safe: it goes on the Edge's intent",
			want:  "ok | supply=in_transit | rel=supply | ingest=1 capred=0 | supply: waits=1 intent=sent@0 chip=\"-\"",
			build: coAt(coPerPositionCurtained),
			act:   seq(supplyStages), probe: pSupply},
		{name: "S7/the swap parks at a live curtain: nobody pressed for it, so it asks for a press (Q8)",
			want:  "ok | supply=staged | rel=- | ingest=0 capred=0 | supply: waits=1 intent=- chip=\"Release the light curtain at SYN-PRESS, then press RELEASE again.\"",
			build: coAt(coPerPositionCurtained),
			act:   seq(curtainReads(curtainLive), supplyStages), probe: pSupply},
		{name: "S7/the press at a live curtain is remembered and goes when it clears",
			want:  "ok | supply=in_transit | rel=supply | ingest=1 capred=0 | click=held:G6 | supply: waits=1 intent=sent@0 chip=\"-\"",
			build: coAt(coPerPositionCurtained),
			act: seq(curtainReads(curtainLive), supplyStages, pressesSupply,
				curtainReads(curtainSafe), floor), probe: pSupply},
	})
}
