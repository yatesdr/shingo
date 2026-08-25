package engine

import (
	"strconv"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// THE TABLE THIS FILE EXISTS FOR: which PHYSICAL leg of a swap receives the
// operator's release disposition — the one that sets remaining_uop, i.e. which
// bin's manifest Core clears.
//
//	mode                       | leg A (R1) | leg B (R2) | full disposition goes to
//	---------------------------|------------|------------|-------------------------
//	two_robot                  | supply     | evac       | leg B  (the evac)
//	press-index, unflipped     | EVAC       | SUPPLY     | leg A  (the evac)
//	press-index, FLIPPED       | EVAC       | SUPPLY     | leg A  (the evac)
//
// The rule is one sentence — the leg that takes the bin OFF the press gets it —
// and the point of the table is that leg A is that leg for press-index while
// leg B is that leg for two_robot. store.ResolveSwapPair labels the pair
// POSITIONALLY (staged→evac, active→supply) and applyProducePlan writes
// active=A, staged=B, so the positional answer is "leg B" for every mode. That
// is right for two_robot and INVERTED for both press-index shapes, and the flip
// does not change it: the flip moves the supermarket trip between the legs, not
// the press pickup and dropoff.
//
// Wrong leg means Core clears the manifest of the bin being placed ONTO the
// press instead of the one coming off it — the ALN_002 shape.
//
// CONSUME ROLE, deliberately. The produce branch of ReleaseOrderWithLineside
// discards remaining_uop for both legs, which is what has kept this off the
// plant floor. Consume is where it bites, and it is the role that makes the
// routing observable at all.
func TestReleaseStagedOrders_FullDispositionGoesToTheEvacLeg(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         protocol.SwapMode
		secondPaired string
		flipped      bool
		wantEvacIsA  bool
		why          string
	}{
		{
			name: "two_robot: the evac is leg B", mode: protocol.SwapModeTwoRobot,
			wantEvacIsA: false,
			why:         "A delivers the fresh bin to the press, B carries the spent one away",
		},
		{
			name: "press-index 2-pos: the evac is leg A", mode: protocol.SwapModeTwoRobotPressIndex,
			wantEvacIsA: true,
			why:         "R1 lifts the spent bin off the press; R2 indexes the fresh one on",
		},
		{
			name: "press-index 3-pos: the evac is still leg A", mode: protocol.SwapModeTwoRobotPressIndex,
			secondPaired: "INDEX-C", wantEvacIsA: true,
			why: "R2's dropoff at the press has no later pickup there, so R2 is the supply even though it ends at the index node",
		},
		{
			name: "press-index FLIPPED 2-pos: the evac is still leg A", mode: protocol.SwapModeTwoRobotPressIndex,
			flipped: true, wantEvacIsA: true,
			why: "the flip moves the supermarket trip to R2; the press pickup stays on R1",
		},
		{
			name: "press-index FLIPPED 3-pos: the evac is still leg A", mode: protocol.SwapModeTwoRobotPressIndex,
			secondPaired: "INDEX-C", flipped: true, wantEvacIsA: true,
			why: "three dropoffs on R2 and only the press one is never picked back up",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testEngineDB(t)
			eng, nodeID, legA, legB := seedConsumeSwapPair(t, db, tc.mode, tc.secondPaired, tc.flipped)

			err := eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{
				Mode: DispositionCaptureLineside, CalledBy: "disposition-routing-test",
			})
			testutil.MustNoErr(t, err, "release staged orders")

			// The manifest disposition is visible as remaining_uop on the wire:
			// capture_lineside with no captures sends &0 (clear the manifest),
			// and the bare disposition the other leg gets sends nil.
			gotA := releaseRemainingUOPFor(t, db, legA)
			gotB := releaseRemainingUOPFor(t, db, legB)

			evacUUID, supplyUUID := legB, legA
			gotEvac, gotSupply := gotB, gotA
			if tc.wantEvacIsA {
				evacUUID, supplyUUID = legA, legB
				gotEvac, gotSupply = gotA, gotB
			}
			if gotEvac == nil || *gotEvac != 0 {
				t.Errorf("the EVAC leg (%s) got remaining_uop=%s, want &0 — %s",
					evacUUID, uopStr(gotEvac), tc.why)
			}
			if gotSupply != nil {
				t.Errorf("the SUPPLY leg (%s) got remaining_uop=%d, want nil — clearing it wipes the "+
					"manifest of the bin being placed on the press. %s", supplyUUID, *gotSupply, tc.why)
			}
		})
	}
}

// A pair whose steps cannot classify keeps the positional labels rather than
// losing the release. Today's behaviour, no worse — and the alternative is
// taking the operator's only release route away over a classification detail.
func TestClassifySwapLegsBySteps_UnclassifiablePairFallsBack(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _, _ := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	eng := testEngine(t, db)

	for _, tc := range []struct {
		name      string
		aSteps    []protocol.ComplexOrderStep
		bSteps    []protocol.ComplexOrderStep
		blankNode bool
		reason    string
	}{
		{
			name:   "neither leg places a bin at the press",
			aSteps: []protocol.ComplexOrderStep{{Action: protocol.ActionDropoff, Node: "ELSEWHERE"}},
			bSteps: []protocol.ComplexOrderStep{{Action: protocol.ActionDropoff, Node: "ELSEWHERE"}},
			reason: "not a supply/evac pair",
		},
		{
			name:   "both legs place a bin at the press",
			aSteps: []protocol.ComplexOrderStep{{Action: protocol.ActionDropoff, Node: "PRESS"}},
			bSteps: []protocol.ComplexOrderStep{{Action: protocol.ActionDropoff, Node: "PRESS"}},
			reason: "not a supply/evac pair",
		},
		{
			name:      "no process node to ask about",
			aSteps:    []protocol.ComplexOrderStep{{Action: protocol.ActionDropoff, Node: "PRESS"}},
			bSteps:    []protocol.ComplexOrderStep{{Action: protocol.ActionPickup, Node: "PRESS"}},
			blankNode: true,
			reason:    "no node name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := mkSwapLeg(t, db, nodeID, "uuid-fb-a-"+tc.name, tc.aSteps, "")
			b := mkSwapLeg(t, db, nodeID, "uuid-fb-b-"+tc.name, tc.bSteps, "")
			node := "PRESS"
			if tc.blankNode {
				node = ""
			}
			if _, _, ok := eng.classifySwapLegsBySteps(node, a.ID, b.ID); ok {
				t.Errorf("classified an unclassifiable pair (%s) — the caller would then trust it", tc.reason)
			}
		})
	}
}

// ...and a pair it CAN classify is classified, so the fallback above is not the
// only path the function ever takes.
func TestClassifySwapLegsBySteps_ClassifiesAWellFormedPair(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _, _ := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	eng := testEngine(t, db)

	placesAtPress := mkSwapLeg(t, db, nodeID, "uuid-ok-supply", []protocol.ComplexOrderStep{
		{Action: protocol.ActionPickup, Node: "INDEX-B"},
		{Action: protocol.ActionDropoff, Node: "PRESS"},
	}, "")
	takesFromPress := mkSwapLeg(t, db, nodeID, "uuid-ok-evac", []protocol.ComplexOrderStep{
		{Action: protocol.ActionPickup, Node: "PRESS"},
		{Action: protocol.ActionDropoff, Node: "MARKET"},
	}, "")

	// Handed in the WRONG WAY ROUND on purpose — the supply-placing leg in the
	// evac slot — which is exactly the press-index arrangement.
	evacID, supplyID, ok := eng.classifySwapLegsBySteps("PRESS", placesAtPress.ID, takesFromPress.ID)
	if !ok {
		t.Fatal("a well-formed pair must classify")
	}
	if evacID != takesFromPress.ID {
		t.Errorf("evac = %d, want %d (the leg that picks up FROM the press)", evacID, takesFromPress.ID)
	}
	if supplyID != placesAtPress.ID {
		t.Errorf("supply = %d, want %d (the leg that leaves a bin ON the press)", supplyID, placesAtPress.ID)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

// seedConsumeSwapPair builds a consume-role swap at a press with both legs
// staged, siblings linked, and the runtime slots filled the way applyProducePlan
// fills them: active = leg A, staged = leg B.
//
// The steps come from the REAL builders via BuildSwapDispatch. A hand-written
// fixture here could only restate what its author believed the shapes were, and
// the shapes are the whole question.
func seedConsumeSwapPair(t *testing.T, db *store.DB, mode protocol.SwapMode, secondPaired string, flipped bool) (*Engine, int64, string, string) {
	t.Helper()
	nodeID, node, claim := seedSwapClaim(t, db, mode, secondPaired)

	// Persist the role and the flip, so the release path — which re-reads the
	// claim from the database — sees the same claim the dispatch was built
	// from. Mutating the in-memory copy alone is how a test ends up asserting
	// about a configuration the code never saw.
	_, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:              claim.StyleID,
		CoreNodeName:         claim.CoreNodeName,
		Role:                 protocol.ClaimRoleConsume,
		SwapMode:             mode,
		PayloadCode:          claim.PayloadCode,
		UOPCapacity:          claim.UOPCapacity,
		InboundSource:        claim.InboundSource,
		InboundStaging:       claim.InboundStaging,
		OutboundStaging:      claim.OutboundStaging,
		OutboundDestination:  claim.OutboundDestination,
		PairedCoreNode:       claim.PairedCoreNode,
		SecondPairedCoreNode: secondPaired,
		IndexRobotSupplies:   &flipped,
	})
	testutil.MustNoErr(t, err, "persist consume claim")
	claim = findActiveClaim(db, node)
	if claim == nil || claim.Role != protocol.ClaimRoleConsume || claim.IndexRobotSupplies != flipped {
		t.Fatalf("claim seed did not take: %+v", claim)
	}

	disp, err := BuildSwapDispatch(node, claim)
	testutil.MustNoErr(t, err, "build swap dispatch")
	if len(disp.StepsA) == 0 || len(disp.StepsB) == 0 {
		t.Fatal("builder produced a one-legged swap — the seed is missing a required field")
	}

	uuidA, uuidB := "uuid-disp-a", "uuid-disp-b"
	legA := mkSwapLeg(t, db, nodeID, uuidA, disp.StepsA, "")
	legB := mkSwapLeg(t, db, nodeID, uuidB, disp.StepsB, "")
	testutil.MustNoErr(t, db.LinkOrderSiblings(legA.ID, legB.ID), "link siblings")
	for _, id := range []int64{legA.ID, legB.ID} {
		testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(protocol.StatusStaged)), "stage leg")
	}
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	// active = A, staged = B — the mapping applyProducePlan writes and
	// ResolveSwapPair reads.
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &legA.ID, &legB.ID), "runtime slots")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claim.ID, 0), "bind claim to runtime")

	eng := testEngine(t, db)
	// Clear anything the seeding queued, so the release envelopes are the only
	// ones on the wire.
	pending, _ := db.ListPendingOutbox(200)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}
	return eng, nodeID, uuidA, uuidB
}

// releaseRemainingUOPFor finds the OrderRelease envelope for one leg and
// returns its remaining_uop. Fails when the leg was not released at all — "no
// envelope" and "nil remaining_uop" are different answers and only one of them
// is about the disposition.
func releaseRemainingUOPFor(t *testing.T, db *store.DB, orderUUID string) *int {
	t.Helper()
	for _, msg := range findOutboxByType(t, db, protocol.TypeOrderRelease) {
		rel := decodeOrderRelease(t, msg)
		if rel.OrderUUID == orderUUID {
			return rel.RemainingUOP
		}
	}
	t.Fatalf("no OrderRelease envelope for leg %s — it was not released, so this test says nothing about its disposition", orderUUID)
	return nil
}

func uopStr(v *int) string {
	if v == nil {
		return "nil"
	}
	return "&" + strconv.Itoa(*v)
}

// THE PRODUCE SIDE, which is where the inversion is live today.
//
// The produce branch discards remaining_uop, so the manifest consequence above
// cannot bite. What it does instead is decide a TRIGGER: ReleaseOrderWithLineside
// fires MaybeCreateUnloaderFullIn on (not supply) AND capture_lineside. Under the
// positional labels neither leg of a press-index pair satisfies both — the real
// evac is handed a disposition with no Mode, and the real supply is caught by
// isSupply — so a press-index press has never fired the downstream unloader
// full-in that a two_robot press fires on every swap.
//
// Asserted through the release breadcrumb rather than the trigger itself: the
// breadcrumb names the disposition each leg was handed, which is the routing
// decision under test, and it does not need an unloader configured to be true.
func TestReleaseStagedOrders_ProduceEvacLegCarriesTheDisposition(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	disp, err := BuildSwapDispatch(node, claim)
	testutil.MustNoErr(t, err, "build swap dispatch")

	legA := mkSwapLeg(t, db, nodeID, "uuid-prod-a", disp.StepsA, "")
	legB := mkSwapLeg(t, db, nodeID, "uuid-prod-b", disp.StepsB, "")
	testutil.MustNoErr(t, db.LinkOrderSiblings(legA.ID, legB.ID), "link siblings")
	for _, id := range []int64{legA.ID, legB.ID} {
		testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(protocol.StatusStaged)), "stage leg")
	}
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &legA.ID, &legB.ID), "runtime slots")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claim.ID, 0), "bind claim")

	eng := testEngine(t, db)
	releaseLogs := captureReleaseLogs(t, eng)
	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{
		Mode: DispositionCaptureLineside, CalledBy: "produce-routing-test",
	}), "release staged orders")

	// Leg A is R1 — the leg that lifts the spent bin off the press.
	wantOn := "order=" + strconv.FormatInt(legA.ID, 10)
	wantOff := "order=" + strconv.FormatInt(legB.ID, 10)
	var sawOn, sawOff bool
	for _, line := range releaseLogs() {
		if !containsAll(line, "produce_role") {
			continue
		}
		if containsAll(line, wantOn, `disposition="capture_lineside"`) {
			sawOn = true
		}
		if containsAll(line, wantOff, `disposition="capture_lineside"`) {
			sawOff = true
		}
	}
	if !sawOn {
		t.Errorf("the EVAC leg (order %d, R1) did not receive the operator's capture_lineside disposition — "+
			"that is the leg the unloader full-in trigger reads.\nlogs: %v", legA.ID, releaseLogs())
	}
	if sawOff {
		t.Errorf("the SUPPLY leg (order %d, R2) received the full disposition — it is the bin going ONTO "+
			"the press and must get the bare one.\nlogs: %v", legB.ID, releaseLogs())
	}
}
