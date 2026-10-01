package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingoedge/release"
	storeorders "shingoedge/store/orders"
)

// release_s7_test.go — S7's pins: a creation that carries a bin across a
// curtained node gets a station wait in front and its intent at creation.

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

// pBackfill renders Order B: its steps' waits, its intent and its chip.
func pBackfill(h *relHarness) []string {
	b := backfillOf(h)
	if b == nil {
		return []string{"B=none"}
	}
	in, _ := release.DecodeIntent(b.ReleaseIntent)
	intent := "-"
	switch {
	case in != nil && in.Sent():
		intent = fmt.Sprintf("sent@%d", in.StationWait)
	case in != nil:
		intent = fmt.Sprintf("held@%d system=%v", in.StationWait, in.System)
	}
	steps, _ := h.db.GetOrderStepsJSON(b.ID)
	waits := 0
	for _, s := range decodeStepsOrNil(steps) {
		if s.Action == protocol.ActionWait {
			waits++
		}
	}
	chip := b.ReleaseHeld
	if chip == "" {
		chip = "-"
	}
	return []string{fmt.Sprintf("B: waits=%d intent=%s chip=%q", waits, intent, chip)}
}

func decodeStepsOrNil(raw string) []protocol.ComplexOrderStep {
	s, err := decodeSteps(raw)
	if err != nil {
		return nil
	}
	return s
}

// backfillStages is Core parking Order B at its first station wait.
func backfillStages(h *relHarness) error {
	b := backfillOf(h)
	if b == nil {
		return fmt.Errorf("no Order B")
	}
	n := 0
	h.handler.HandleOrderStaged(&protocol.Envelope{}, &protocol.OrderStaged{OrderUUID: b.UUID, Detail: "stub: parked",
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

// pressesB is an operator's RELEASE on Order B, which must hold.
func pressesB(h *relHarness) error {
	b := backfillOf(h)
	if b == nil {
		return fmt.Errorf("no Order B")
	}
	return held(func(h *relHarness) error { return h.eng.ReleaseOrderWithLineside(b.ID, dispNone) })(h)
}

func TestReleaseS7Pins(t *testing.T) {
	t.Parallel()
	runRelCells(t, []relCell{
		{name: "S7/sequential B on a curtained line: a wait in front, the Edge's own intent",
			want:  "ok | removal=in_transit B=submitted | rel=removal | ingest=1 capred=0 | B: waits=1 intent=held@0 system=true chip=\"-\"",
			build: seqWithCurtain(curtainSafe),
			act:   seq(orderClick("removal", dispEmpty), namesB), probe: pBackfill},
		{name: "S7/B parks with the curtain safe: it goes on the Edge's intent",
			want:  "ok | removal=in_transit B=in_transit | rel=removal,B | ingest=1 capred=0 | B: waits=1 intent=sent@0 chip=\"-\"",
			build: seqWithCurtain(curtainSafe),
			act:   seq(orderClick("removal", dispEmpty), namesB, backfillStages), probe: pBackfill},
		{name: "S7/B parks at a live curtain: nobody pressed for it, so it asks for a press (Q8)",
			want:  "ok | removal=in_transit B=staged | rel=removal | ingest=1 capred=0 | B: waits=1 intent=- chip=\"Release the light curtain at SYN-PRESS, then press RELEASE again.\"",
			build: seqWithCurtain(curtainSafe),
			act:   seq(orderClick("removal", dispEmpty), namesB, curtainReads(curtainLive), backfillStages), probe: pBackfill},
		{name: "S7/the press at a live curtain is remembered and goes when it clears",
			want:  "ok | removal=in_transit B=in_transit | rel=removal,B | ingest=1 capred=0 | click=held:G6 | B: waits=1 intent=sent@0 chip=\"-\"",
			build: seqWithCurtain(curtainSafe),
			act: seq(orderClick("removal", dispEmpty), namesB, curtainReads(curtainLive), backfillStages, pressesB,
				curtainReads(curtainSafe), floor), probe: pBackfill},
		{name: "S7/control: no curtain, no wait, no intent",
			want: "ok | removal=in_transit B=submitted | rel=removal | ingest=1 capred=0 | B: waits=0 intent=- chip=\"-\"",
			build: func(h *relHarness) {
				h.sequentialAB(protocol.ClaimRoleProduce, true)
				statuses(h, "removal", protocol.StatusStaged)
			},
			act: seq(orderClick("removal", dispEmpty), namesB), probe: pBackfill},
	})
}
