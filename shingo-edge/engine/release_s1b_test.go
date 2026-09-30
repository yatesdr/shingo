package engine

// release_s1b_test.go — the produce count splits at the operator's RELEASE
// (owner, 2026-09-30). Parts made before RELEASE, including after the call for
// parts, belong to the departing bin; parts made after it belong to the next
// bin. Each cell drives a whole cycle through the real doors — the request, the
// PLC ticks through the counter path, the release, Core's lift and the next
// bin's delivery — and prints every ingest the cycle shipped and where the
// count stands.
//
// A held count (pending_uop_delta) replays onto the next bin on its first tick
// after the bind, so a delivered bin reads "uop=0 pending=N": N is what it will
// carry.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// ticks runs n parts through the counter path, as the PLC reports them: the
// active-pull logic, hold-and-replay and the accumulator are the real ones.
func ticks(n int) func(h *relHarness) error {
	return func(h *relHarness) error {
		h.t.Helper()
		p, err := h.db.GetProcess(h.processID)
		testutil.MustNoErr(h.t, err, "process")
		if p.ActiveStyleID == nil {
			h.t.Fatal("fixture: process has no active style")
		}
		for i := 0; i < n; i++ {
			h.eng.handleCounterDelta(CounterDeltaEvent{ProcessID: h.processID, StyleID: *p.ActiveStyleID, Delta: 1})
		}
		return nil
	}
}

// requestProduce is the operator's call for parts at the front node; the leg
// that lifts the front's bin is named by its steps.
func requestProduce(name string) func(h *relHarness) error {
	return func(h *relHarness) error {
		h.t.Helper()
		res, err := h.eng.RequestProduceSwap(h.nodeID)
		testutil.MustNoErr(h.t, err, "request")
		o := res.Order
		if o == nil {
			o = res.OrderA
		}
		h.addLeg(name, o.ID)
		return nil
	}
}

// liftedAt delivers Core's BinPickedUp for a leg lifting the named node's
// original bin.
func liftedAt(leg, node string, bin int64) func(h *relHarness) error {
	return func(h *relHarness) error {
		h.eng.HandleBinPickedUp(h.order(leg).UUID, bin, node)
		return nil
	}
}

// delivered binds a new bin at a node the way the delivery handler does
// (uop.Mutator.OnDelivered): the claim the runtime holds, the bin, epoch 1,
// Core's count 0 for an empty.
func delivered(nodeID func(h *relHarness) int64, bin int64) func(h *relHarness) error {
	return func(h *relHarness) error {
		h.t.Helper()
		id := nodeID(h)
		rt, err := h.db.GetProcessNodeRuntime(id)
		testutil.MustNoErr(h.t, err, "runtime")
		testutil.MustNoErr(h.t, h.eng.inventoryDelta.(interface {
			OnDelivered(nodeID int64, activeClaimID *int64, binID int64, deltaEpoch int64, uop int) error
		}).OnDelivered(id, rt.ActiveClaimID, bin, 1, 0), "bind delivered bin")
		return nil
	}
}

func front(h *relHarness) int64 { return h.nodeID }
func back(h *relHarness) int64  { return h.partnerID }

// pIngests names every ingest the act shipped: its count, the bin it pinned and
// the payload it named.
func pIngests(h *relHarness) []string {
	var out []string
	for _, m := range h.sinceMark() {
		if m.MsgType != protocol.TypeOrderIngest {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(h.t, json.Unmarshal(m.Payload, &env), "ingest envelope")
		var p protocol.OrderIngestRequest
		testutil.MustNoErr(h.t, env.DecodePayload(&p), "ingest payload")
		out = append(out, fmt.Sprintf("qty=%d bin=%d payload=%s", p.Quantity, p.BinID, p.PayloadCode))
	}
	return []string{"ingests=[" + strings.Join(out, "; ") + "]"}
}

// pSlot prints a node's count, held count and bound bin.
func pSlot(label string, nodeID func(h *relHarness) int64) func(h *relHarness) []string {
	return func(h *relHarness) []string {
		rt, err := h.db.GetProcessNodeRuntime(nodeID(h))
		testutil.MustNoErr(h.t, err, "runtime "+label)
		bin := "nil"
		if rt.ActiveBinID != nil {
			bin = fmt.Sprint(*rt.ActiveBinID)
		}
		return []string{fmt.Sprintf("%s uop=%d pending=%d bin=%s", label, rt.RemainingUOPCached, rt.PendingUOPDelta, bin)}
	}
}

func TestReleaseS1bProduceSplit(t *testing.T) {
	t.Parallel()
	S := protocol.StatusStaged
	twoRobot := pairSpec{mode: protocol.SwapModeTwoRobot}
	runRelCells(t, []relCell{
		// two_robot splits at the pair click today, which is the rule.
		{name: "S1b/two_robot: click, 5 ticks, Core refuses, re-click, lift, supply delivered",
			want: "ok | evac=in_transit supply=in_transit | rel=evac,supply,evac,supply | ingest=1 capred=0 | " +
				"ingests=[qty=42 bin=9001 payload=PART-X] | A uop=0 pending=5 bin=9002",
			build: pairAt(twoRobot, "evac", S, "supply", S),
			act: seq(pairClick(dispEmpty), ticks(5), refuse("evac", "invalid_state"), pairClick(dispEmpty),
				liftedAt("evac", fxPress, fxBin), delivered(front, fxBin+1)),
			probe: probes(pIngests, pSlot("A", front))},

		{name: "S1b/single_robot: request, 5 ticks, RELEASE, 3 ticks, lift, delivered",
			bug:   "S1b",
			today: "ok | swap=in_transit | rel=swap | ingest=1 capred=0 | ingests=[qty=42 bin=0 payload=PART-X] | A uop=0 pending=8 bin=9002",
			want:  "ok | swap=in_transit | rel=swap | ingest=1 capred=0 | ingests=[qty=47 bin=9001 payload=PART-X] | A uop=0 pending=3 bin=9002",
			build: func(h *relHarness) { h.seedNode(pairSpec{mode: protocol.SwapModeSingleRobot}) },
			act: seq(requestProduce("swap"), ticks(5), stages("swap"), orderClick("swap", dispEmpty), ticks(3),
				liftedAt("swap", fxPress, fxBin), delivered(front, fxBin+1)),
			probe: probes(pIngests, pSlot("A", front))},

		{name: "S1b/sequential: request, 5 ticks, RELEASE (flip), 3 ticks",
			bug:   "S1b",
			today: "ok | removal=in_transit | rel=removal | ingest=1 capred=0 | ingests=[qty=42 bin=0 payload=PART-X] | A uop=0 pending=5 bin=nil | B uop=45 pending=0 bin=9002",
			want:  "ok | removal=in_transit | rel=removal | ingest=1 capred=0 | ingests=[qty=47 bin=9001 payload=PART-X] | A uop=0 pending=0 bin=nil | B uop=45 pending=0 bin=9002",
			build: func(h *relHarness) { h.sequentialPair(protocol.ClaimRoleProduce, true) },
			act: seq(requestProduce("removal"), ticks(5), stages("removal"), orderClick("removal", dispEmpty),
				ticks(3)),
			probe: probes(pIngests, pSlot("A", front), pSlot("B", back))},

		{name: "S1b/sequential, B has no bin: request, 5 ticks, RELEASE, 3 ticks, B's bin delivered",
			bug:   "S1b",
			today: "ok | removal=in_transit | rel=removal | ingest=1 capred=0 | ingests=[qty=42 bin=0 payload=PART-X] | A uop=0 pending=5 bin=nil | B uop=0 pending=3 bin=9003",
			want:  "ok | removal=in_transit | rel=removal | ingest=1 capred=0 | ingests=[qty=47 bin=9001 payload=PART-X] | A uop=0 pending=0 bin=nil | B uop=0 pending=3 bin=9003",
			build: func(h *relHarness) { h.sequentialPair(protocol.ClaimRoleProduce, false) },
			act: seq(requestProduce("removal"), ticks(5), stages("removal"), orderClick("removal", dispEmpty),
				ticks(3), delivered(back, fxBin+2)),
			probe: probes(pIngests, pSlot("A", front), pSlot("B", back))},

		// A changeover evac of a produce node ships no manifest today (R4-1).
		{name: "S1b/changeover evac produce→produce, node click",
			bug:   "S1b",
			today: "ok | evac=in_transit supply=staged | rel=evac | ingest=0 capred=0 | ingests=[] | A uop=42 pending=0 bin=9001",
			want:  "ok | evac=in_transit supply=staged | rel=evac | ingest=1 capred=0 | ingests=[qty=42 bin=9001 payload=PART-X] | A uop=0 pending=0 bin=nil",
			build: coAt(coSpec{mode: protocol.SwapModeTwoRobot}, "evac", S, "supply", S),
			act:   orderClick("evac", dispEmpty), probe: probes(pIngests, pSlot("A", front))},
		{name: "S1b/changeover evac produce→consume, node click",
			bug:   "S1b",
			today: "ok | evac=in_transit supply=staged | rel=evac | ingest=0 capred=0 | ingests=[] | A uop=0 pending=0 bin=9001",
			want:  "ok | evac=in_transit supply=staged | rel=evac | ingest=1 capred=0 | ingests=[qty=42 bin=9001 payload=PART-X] | A uop=0 pending=0 bin=nil",
			build: coAt(coSpec{mode: protocol.SwapModeTwoRobot, toRole: protocol.ClaimRoleConsume}, "evac", S, "supply", S),
			act:   orderClick("evac", dispEmpty), probe: probes(pIngests, pSlot("A", front))},
		{name: "S1b/changeover evac produce→produce, the plant-wide sweep",
			bug:   "S1b",
			today: "ok | evac=in_transit supply=staged | rel=evac | ingest=0 capred=0 | sweep released=1 pending=0 deferred=1 flip=[] | ingests=[] | A uop=42 pending=0 bin=9001",
			want:  "ok | evac=in_transit supply=staged | rel=evac | ingest=1 capred=0 | sweep released=1 pending=0 deferred=1 flip=[] | ingests=[qty=42 bin=9001 payload=PART-X] | A uop=0 pending=0 bin=nil",
			build: coAt(coSpec{mode: protocol.SwapModeTwoRobot}, "evac", S, "supply", S),
			act:   sweepClick(dispNone), probe: probes(pIngests, pSlot("A", front))},
		{name: "S1b/changeover drop of a produce node, node click",
			bug:   "S1b",
			today: "ok | evac=in_transit | rel=evac | ingest=0 capred=0 | ingests=[] | A uop=42 pending=0 bin=9001",
			want:  "ok | evac=in_transit | rel=evac | ingest=1 capred=0 | ingests=[qty=42 bin=9001 payload=PART-X] | A uop=0 pending=0 bin=nil",
			build: coAt(coSpec{mode: protocol.SwapModeTwoRobot, drop: true}, "evac", S),
			act:   orderClick("evac", dispEmpty), probe: probes(pIngests, pSlot("A", front))},
	})
}
