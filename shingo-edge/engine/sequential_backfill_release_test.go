package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/release"
)

// sequential_backfill_release_test.go — when a sequential position's Order B
// (the backfill robot) is created, walked through the messages Core sends for
// Order A (the removal) and the station's RELEASE at the line: after that
// release, never before (owner, 2026-11-15).

// coreWaybill is Core's order.waybill for a leg, through the Edge's handler.
func coreWaybill(h *relHarness, name, robot string) {
	o := h.order(name)
	h.handler.HandleOrderWaybill(&protocol.Envelope{}, &protocol.OrderWaybill{OrderUUID: o.UUID, WaybillID: "wb-" + name, RobotID: robot})
}

// coreStagedAt is Core's order.staged for a leg at its station wait n.
func coreStagedAt(h *relHarness, name string, n int) {
	o := h.order(name)
	h.handler.HandleOrderStaged(&protocol.Envelope{}, &protocol.OrderStaged{OrderUUID: o.UUID, Detail: "robot dwelling at staging node",
		StationWait: &n, WaitKind: protocol.WaitKindStation})
}

// backfills is every order the harness's front position has been sent as its
// backfill: the ones dropping at it.
func backfills(h *relHarness) []int64 { return ordersDroppingAt(h.t, h.db, fxPress) }

// TestSequentialBackfill_WhenTheSecondRobotIsCalled: a sequential position,
// consume (PRS_010's shape) and produce. Order B does not exist when Core names
// A's robot (its waybill, which made B before this change) nor when A parks at
// the line; the station's RELEASE there makes exactly one, attributed to A's
// demand and linked to A as the handler always made it.
func TestSequentialBackfill_WhenTheSecondRobotIsCalled(t *testing.T) {
	t.Parallel()
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			h := newRelHarness(t)
			h.sequentialAB(role, true)

			coreWaybill(h, "removal", "AMR-1")
			if b := backfills(h); len(b) != 0 {
				t.Fatalf("after A's waybill: backfills %v, want none (A has not reached the line)", b)
			}
			coreStagedAt(h, "removal", 0)
			if b := backfills(h); len(b) != 0 {
				t.Fatalf("A parked at the line, not released: backfills %v, want none", b)
			}
			testutil.MustNoErr(t, orderClick("removal", dispEmpty)(h), "the station's RELEASE at the line")

			b := backfills(h)
			if len(b) != 1 {
				t.Fatalf("after the release at the line: backfills %v, want exactly one", b)
			}
			a, bo := h.order("removal"), backfillOf(h)
			if bo == nil || bo.ID != b[0] || bo.SiblingOrderID == nil || *bo.SiblingOrderID != a.ID {
				t.Errorf("B %v is not linked to A %d both ways", bo, a.ID)
			}
			if bo != nil && (bo.OriginID != a.OriginID || bo.OriginClass != a.OriginClass) {
				t.Errorf("B's origin %q/%q, want A's %q/%q", bo.OriginID, bo.OriginClass, a.OriginID, a.OriginClass)
			}
			rt, err := h.db.GetProcessNodeRuntime(h.nodeID)
			testutil.MustNoErr(t, err, "runtime")
			if rt.StagedOrderID == nil || *rt.StagedOrderID != b[0] {
				t.Errorf("the position's staged order is %v, want B %d", rt.StagedOrderID, b[0])
			}
		})
	}
}

// TestSequentialBackfill_ARejectedReleaseMakesNoSecondB: Core refuses A's
// release after B was made, A rolls back to staged, and the station presses
// again. The first B stays (it is on its way with the new bin); the second
// release makes no other.
func TestSequentialBackfill_ARejectedReleaseMakesNoSecondB(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	h.sequentialAB(protocol.ClaimRoleConsume, true)
	coreWaybill(h, "removal", "AMR-1")
	coreStagedAt(h, "removal", 0)
	testutil.MustNoErr(t, orderClick("removal", dispEmpty)(h), "the first RELEASE")
	first := backfills(h)
	if len(first) != 1 {
		t.Fatalf("after the first release: backfills %v, want one", first)
	}

	h.handler.HandleOrderError(&protocol.Envelope{}, &protocol.OrderError{OrderUUID: h.order("removal").UUID,
		ErrorCode: "invalid_state", Detail: "the fleet did not take the release"})
	if got := h.order("removal").Status; got != protocol.StatusStaged {
		t.Fatalf("after Core's refusal A is %s, want staged (rolled back)", got)
	}
	coreStagedAt(h, "removal", 0)
	testutil.MustNoErr(t, orderClick("removal", dispEmpty)(h), "the RELEASE again")

	if b := backfills(h); len(b) != 1 || b[0] != first[0] {
		t.Errorf("after the second release: backfills %v, want only the first %v", b, first)
	}
	if o, err := h.db.GetOrder(first[0]); err != nil || protocol.IsTerminal(o.Status) {
		t.Errorf("the first B is %v (%v), want it live: it finishes its trip", o, err)
	}
}

// TestSequentialBackfill_AReleaseWhileADrives: a release of A sent while its
// robot still drives to the line. The board does not offer one (the purpose
// reads present, not ready, for a driving leg: service/release_render.go);
// the order's own door takes it, and Core lets the robot through the line wait
// without stopping, so it is the release at the line: B is made at it.
func TestSequentialBackfill_AReleaseWhileADrives(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	h.sequentialAB(protocol.ClaimRoleConsume, true)
	coreWaybill(h, "removal", "AMR-1")

	err := orderClick("removal", dispEmpty)(h)
	if b := backfills(h); err != nil || len(b) != 1 {
		t.Errorf("a release while A drives: err %v, backfills %v; want it sent and one B", err, b)
	}
}

// TestSequentialBackfill_ARestartBetweenTheReleaseAndB: the release of A went
// out (its row in transit, its intent sent) and this Edge stopped before B was
// made. The start makes it, once.
func TestSequentialBackfill_ARestartBetweenTheReleaseAndB(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	h.sequentialAB(protocol.ClaimRoleConsume, true)
	coreWaybill(h, "removal", "AMR-1")
	coreStagedAt(h, "removal", 0)
	a := h.leg("removal")
	testutil.MustNoErr(t, h.db.SetOrderReleaseIntent(a, `{"station_wait":0,"sent_at":"2026-11-15T00:00:00Z"}`), "the release's intent, sent")
	testutil.MustNoErr(t, h.db.UpdateOrderStatus(a, string(orders.StatusInTransit)), "the release's in_transit")
	if b := backfills(h); len(b) != 0 {
		t.Fatalf("fixture: backfills %v before the start, want none", b)
	}

	h.eng.recoverSequentialBackfills()
	h.eng.recoverSequentialBackfills()

	if b := backfills(h); len(b) != 1 {
		t.Errorf("after the start: backfills %v, want exactly one", b)
	}
}

// TestSequentialBackfill_ACancelledBIsNotMadeAgainAtAStart: B was made at A's
// release and then cancelled (here by Core) while A still drives, which clears
// the position's pointer to it. A start does not make another: A was given its B.
func TestSequentialBackfill_ACancelledBIsNotMadeAgainAtAStart(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	h.sequentialAB(protocol.ClaimRoleConsume, true)
	coreWaybill(h, "removal", "AMR-1")
	coreStagedAt(h, "removal", 0)
	testutil.MustNoErr(t, orderClick("removal", dispEmpty)(h), "the RELEASE at the line")
	b := backfillOf(h)
	if b == nil {
		t.Fatal("fixture: no B after the release")
	}
	h.handler.HandleOrderCancelled(&protocol.Envelope{}, &protocol.OrderCancelled{OrderUUID: b.UUID, Reason: "cancelled at Core"})
	rt, err := h.db.GetProcessNodeRuntime(h.nodeID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.StagedOrderID != nil {
		t.Fatalf("fixture: the position still points at B %d after its cancel", *rt.StagedOrderID)
	}

	h.eng.recoverSequentialBackfills()

	if got := backfills(h); len(got) != 1 || got[0] != b.ID {
		t.Errorf("after the start: backfills %v, want only the cancelled B %d", got, b.ID)
	}
}

// TestSequentialBackfill_NoneForARemovalEndedBeforeItsRelease: A cancelled at
// the line before anyone released it, by a person or by Core. No B, now or at
// the next start.
func TestSequentialBackfill_NoneForARemovalEndedBeforeItsRelease(t *testing.T) {
	t.Parallel()
	for _, end := range []struct {
		name string
		do   func(h *relHarness)
	}{
		{"a person's abort", func(h *relHarness) {
			testutil.MustNoErr(h.t, h.eng.orderMgr.AbortOrder(h.leg("removal")), "abort A")
		}},
		{"Core's cancel", func(h *relHarness) {
			h.handler.HandleOrderCancelled(&protocol.Envelope{}, &protocol.OrderCancelled{OrderUUID: h.order("removal").UUID, Reason: "cancelled at Core"})
		}},
	} {
		t.Run(end.name, func(t *testing.T) {
			t.Parallel()
			h := newRelHarness(t)
			h.sequentialAB(protocol.ClaimRoleConsume, true)
			coreWaybill(h, "removal", "AMR-1")
			coreStagedAt(h, "removal", 0)
			end.do(h)
			h.eng.recoverSequentialBackfills()
			if got := h.order("removal").Status; !protocol.IsTerminal(got) {
				t.Fatalf("fixture: A is %s, want ended", got)
			}
			if b := backfills(h); len(b) != 0 {
				t.Errorf("backfills %v after A ended unreleased, want none", b)
			}
		})
	}
}

// TestSequentialBackfill_FollowsTheReleaseAtTheLineNotAnEarlierWait: an Order A
// whose plan has a station wait at another node before the one at the line.
// Its release there makes no B; its release at the line does.
func TestSequentialBackfill_FollowsTheReleaseAtTheLineNotAnEarlierWait(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	h.sequentialPair(protocol.ClaimRoleConsume, true)
	steps := []protocol.ComplexOrderStep{
		stationWait("SYN-GATE", release.PurposeSwap),
		stationWait(fxPress, release.PurposeSwap),
		{Action: protocol.ActionPickup, Node: fxPress},
		{Action: protocol.ActionDropoff, Node: "SYN-MARKET"},
	}
	nodeID := h.nodeID
	a, err := h.eng.orderMgr.CreateComplexOrder(&nodeID, 1, "", fxPress, steps, orders.NoDemand())
	testutil.MustNoErr(t, err, "create A")
	testutil.MustNoErr(t, h.db.UpdateProcessNodeRuntimeOrders(nodeID, &a.ID, nil), "A is the position's active order")
	h.addLeg("removal", a.ID)

	coreWaybill(h, "removal", "AMR-1")
	coreStagedAt(h, "removal", 0)
	testutil.MustNoErr(t, orderClick("removal", dispEmpty)(h), "the RELEASE at the first wait")
	if b := backfills(h); len(b) != 0 {
		t.Fatalf("after the release at the earlier wait: backfills %v, want none", b)
	}
	coreStagedAt(h, "removal", 1)
	testutil.MustNoErr(t, orderClick("removal", dispEmpty)(h), "the RELEASE at the line")
	if b := backfills(h); len(b) != 1 {
		t.Errorf("after the release at the line: backfills %v, want one", b)
	}
}

// TestSequentialOneNode_SteadyState: a Sequential A/B position with no
// partner, a one-node line (final brief 2a, point 1): HOP's consume line, and
// its produce dual, through the doors a plant uses. Every tick is charged to
// the one node's bin whatever its active_pull bit reads (it is set dark, where
// the parked-side skip would bite on a pair); Order B does not exist when Core
// names A's robot or when A waits at the line, and the station's RELEASE there
// makes exactly one, linked to A; and nothing on the way writes active_pull.
// The paired press is the control: its release moves the line, so the
// statement scan sees a write where there is one.
func TestSequentialOneNode_SteadyState(t *testing.T) {
	t.Parallel()
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			h := newRelHarness(t)
			claim := h.seedNode(pairSpec{mode: protocol.SwapModeSequential, role: role, onePosition: true})
			if claim.PairedCoreNode != "" {
				t.Fatalf("fixture: the claim names partner %q, want none", claim.PairedCoreNode)
			}
			testutil.MustNoErr(t, h.db.SetActivePull(h.nodeID, false), "the node's bit dark")
			proc, err := h.db.GetProcess(h.processID)
			testutil.MustNoErr(t, err, "process")
			sign := -1
			if role == protocol.ClaimRoleProduce {
				sign = 1
			}
			want := fxCount
			tick := func(label string, d int) {
				t.Helper()
				h.eng.handleCounterDelta(CounterDeltaEvent{ProcessID: h.processID, StyleID: *proc.ActiveStyleID, Delta: int64(d)})
				want += sign * d
				if got := runtimeOf(t, h.eng, h.nodeID).RemainingUOPCached; got != want {
					t.Errorf("%s: the node's count is %d, want %d: a one-node line charges every tick to its one bin",
						label, got, want)
				}
			}
			h.counter.Reset()

			tick("a stroke before the request", 3)
			h.requestRemoval(role)
			coreWaybill(h, "removal", "AMR-1")
			coreStagedAt(h, "removal", 0)
			if b := backfills(h); len(b) != 0 {
				t.Fatalf("Order A waits at the line, not released: backfills %v, want none", b)
			}
			tick("a stroke while Order A waits at the line", 2)
			testutil.MustNoErr(t, orderClick("removal", dispEmpty)(h), "the station's RELEASE at the line")

			b := backfills(h)
			if len(b) != 1 {
				t.Fatalf("after the release at the line: backfills %v, want exactly one", b)
			}
			if bo := backfillOf(h); bo == nil || bo.ID != b[0] {
				t.Errorf("B %v is not the one A names as its sibling", bo)
			}
			if w := activePullWrites(h); len(w) != 0 {
				t.Errorf("a one-node line wrote active_pull %d times:\n%s", len(w), strings.Join(w, "\n"))
			}
		})
	}
	t.Run("control: a paired side's release moves the line", func(t *testing.T) {
		t.Parallel()
		h := newRelHarness(t)
		h.sequentialAB(protocol.ClaimRoleConsume, true)
		h.counter.Reset()
		coreWaybill(h, "removal", "AMR-1")
		coreStagedAt(h, "removal", 0)
		testutil.MustNoErr(t, orderClick("removal", dispEmpty)(h), "the station's RELEASE at the line")
		if w := activePullWrites(h); len(w) == 0 {
			t.Errorf("the pair's release wrote no active_pull: the scan cannot see the flip's write")
		}
	})
}

// activePullWrites is every statement the harness's store sent since its last
// Reset that writes the active_pull bit.
func activePullWrites(h *relHarness) []string {
	var out []string
	for _, s := range h.counter.Statements() {
		if strings.Contains(s, "SET active_pull") {
			out = append(out, s)
		}
	}
	return out
}
