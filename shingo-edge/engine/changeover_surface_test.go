package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// changeover_surface_test.go — the node set and the destination surface.
//
// WHICH OF THESE ACTUALLY DEMONSTRATE THE FIX, checked by running them against
// commit 4 rather than assumed. The first draft of this header claimed three;
// the truth is one, and the difference matters because a test that passes for
// the wrong reason is worse than no test.
//
//   - **UnchangedParticipant — FAILS against commit 4.** The real proof. The node
//     is in plan.diffs but the old gate skipped SituationUnchanged, so a carrier
//     moving to a node this changeover touches was invisible.
//   - **IndexedOverSeat — passed against commit 4, for the wrong reason**, so it
//     is asserted on the BLOCKER TEXT now, not merely on "an error happened".
//     Press-index config has its own refusal paths and one of them was firing.
//     It is the Hopkinsville shape (the removed sweep missed orders 1249/1251
//     "only because it walks plan.diffs" while they delivered to PLN_02/PLN_05),
//     and it is worth keeping — but only stated as a guard, not as evidence.
//   - **SimpleOrderElsewhere — passed against commit 4 trivially**, because an
//     order with no runtime slot was invisible to the old surface anyway. It is
//     a guard against a FUTURE legPlacesBinAt-only implementation, where empty
//     steps_json (the DDL default for every simple order) would block
//     unconditionally. Not evidence for this commit.
//
// The fourth pins the pair that must NOT be merged: the block set includes
// unchanged participants, the cancel set does not.

// seedOrderTo creates a live order DELIVERING to a core node, with no runtime
// slot anywhere. That absence is the point: the destination surface must find it
// on its delivery alone, because the runtime pointer is UI state — it is nulled
// while an order is still live, press-index legs live in the head node's slots,
// and primes are deliberately never slotted.
func seedOrderTo(t *testing.T, db *store.DB, uuid, deliveryNode string, complex bool, status protocol.Status) int64 {
	t.Helper()
	typ := orders.TypeMove
	if complex {
		typ = orders.TypeComplex
	}
	id, err := db.CreateOrder(uuid, typ, nil, false, 1, deliveryNode, "", "SOURCE-OLD", "", false, "PART-OLD")
	testutil.MustNoErr(t, err, "create order "+uuid)
	if complex {
		steps := `[{"action":"pickup","node":"SOURCE-OLD"},{"action":"dropoff","node":"` + deliveryNode + `"}]`
		testutil.MustNoErr(t, db.UpdateOrderStepsJSON(id, steps), "store steps")
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(status)), "set status")
	return id
}

// TestBlockNodeSet_IncludesIndexedOverSeatsAndUnchangedNodes is the node-set
// half of the Hopkinsville fix, tested directly.
//
// It is a UNIT test on blockNodeSet rather than an end-to-end changeover,
// deliberately: a press-index changeover cannot even be planned in this harness
// ("Core unavailable; cannot determine bin types"), so an end-to-end version
// refuses before the gate ever runs — which is exactly how the first draft of
// this test passed for the wrong reason. The node set is the thing the fix
// changed; test that, not a refusal that happens to arrive from elsewhere.
//
// The seats are the Hopkinsville blind spot: the removed AbortNodeOrders sweep
// missed orders 1249/1251 "only because it walks plan.diffs (the task nodes)"
// while they were delivering to PLN_02/PLN_05, and the gate that replaced it
// inherited the same walk.
func TestBlockNodeSet_IncludesIndexedOverSeatsAndUnchangedNodes(t *testing.T) {
	t.Parallel()
	plan := &changeoverPlan{
		diffs: []ChangeoverNodeDiff{
			{CoreNodeName: "PLN_01", Situation: SituationSwap},
			{CoreNodeName: "PLN_04", Situation: SituationUnchanged},
		},
		participants: []domain.ParticipantInput{
			{CoreNodeName: "PLN_01", Role: domain.ParticipantRoleTask},
			{CoreNodeName: "PLN_04", Role: domain.ParticipantRoleTask},
			{CoreNodeName: "PLN_02", Role: domain.ParticipantRoleIndexedOver, OwningTaskCoreNode: "PLN_01"},
			{CoreNodeName: "PLN_05", Role: domain.ParticipantRoleIndexedOver, OwningTaskCoreNode: "PLN_04"},
		},
	}

	block := blockNodeSet(plan)
	for _, want := range []string{"PLN_01", "PLN_04", "PLN_02", "PLN_05"} {
		if !containsStr(block, want) {
			t.Errorf("blockNodeSet missing %s — a carrier moving there collides with this "+
				"changeover; %s is the Hopkinsville shape", want, want)
		}
	}

	// The cancel set is deliberately narrower and must stay a separate function.
	cancel := cancelNodeSet(plan)
	if containsStr(cancel, "PLN_02") || containsStr(cancel, "PLN_05") {
		t.Error("cancelNodeSet contains an indexed_over seat — cancelling a carrier bound for a " +
			"seat is the HK 2026-07-28 deadlock the removed sweep would have caused")
	}
	if containsStr(cancel, "PLN_04") {
		t.Error("cancelNodeSet contains a SituationUnchanged node — the incoming style still " +
			"claims that payload there, so its orders are still wanted")
	}
	if !containsStr(cancel, "PLN_01") {
		t.Error("cancelNodeSet missing the changed node PLN_01")
	}
}

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestChangeoverStart_BlocksCarrierBoundForAnUnchangedParticipant — the second
// blind spot. The node IS in plan.diffs, but the gate skipped SituationUnchanged,
// so a carrier moving to a node this changeover touches was invisible. FAILED
// before commit 5.
func TestChangeoverStart_BlocksCarrierBoundForAnUnchangedParticipant(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, fromStyleID, toStyleID, _, _ := seedChangeoverScenario(t, db)

	// A second node both styles claim identically → SituationUnchanged.
	_, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "SAME-NODE", Code: "SAME",
		Name: "Unchanged Node", Sequence: 3, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create unchanged node")
	for _, sid := range []int64{fromStyleID, toStyleID} {
		_, cerr := upsertClaimLegacySimple(db, processes.NodeClaimInput{
			StyleID: sid, CoreNodeName: "SAME-NODE", Role: "consume", SwapMode: "simple",
			PayloadCode: "PART-SAME", UOPCapacity: 100, InboundSource: "SOURCE-OLD",
		})
		testutil.MustNoErr(t, cerr, "upsert unchanged claim")
	}
	_ = nodeID
	eng := testEngine(t, db)

	_ = seedOrderTo(t, db, "uuid-unchanged-carrier", "SAME-NODE", false, orders.StatusInTransit)

	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err == nil {
		t.Fatal("changeover started while a carrier was in transit to a SituationUnchanged " +
			"participant — the changeover still touches that node")
	}
}

// TestChangeoverStart_SimpleOrderElsewhereDoesNotBlock is the rev-2 correction's
// test, and the reason the surface is two-armed rather than legPlacesBinAt alone.
//
// A simple order writes NO steps (steps_json TEXT NOT NULL DEFAULT ”), and
// orderGatesCutover treats empty steps as an unconditional block. Inheriting that
// here would make the gate BROADER for the majority case — every simple order
// anywhere would block every changeover — and the floor complaint would survive
// the fix that was supposed to end it.
func TestChangeoverStart_SimpleOrderElsewhereDoesNotBlock(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)

	// Simple, in_transit, delivering to a node this changeover never touches.
	id := seedOrderTo(t, db, "uuid-elsewhere", "SOMEWHERE-ELSE", false, orders.StatusInTransit)

	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err != nil {
		t.Fatalf("a simple order delivering to an unrelated node blocked the changeover: %v — "+
			"empty steps_json means 'simple, use the delivery-node arm', never 'gate'", err)
	}
	if got := reloadStatus(t, db, id); got != orders.StatusInTransit {
		t.Errorf("unrelated order = %s, want left alone", got)
	}
}

// TestChangeoverStart_UnchangedParticipantBlocksButIsNotCancelled pins the pair
// that must stay two separately named node sets.
//
// Widening the BLOCK to an unchanged participant is right — a carrier is moving
// there. Widening the CANCEL to it is a quiet bug: the incoming style still
// claims that payload at that node, so the order is still wanted, and cancelling
// it would starve the cell the changeover is meant to keep running.
func TestChangeoverStart_UnchangedParticipantBlocksButIsNotCancelled(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, fromStyleID, toStyleID, _, _ := seedChangeoverScenario(t, db)

	_, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "SAME-NODE", Code: "SAME",
		Name: "Unchanged Node", Sequence: 3, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create unchanged node")
	for _, sid := range []int64{fromStyleID, toStyleID} {
		_, cerr := upsertClaimLegacySimple(db, processes.NodeClaimInput{
			StyleID: sid, CoreNodeName: "SAME-NODE", Role: "consume", SwapMode: "simple",
			PayloadCode: "PART-SAME", UOPCapacity: 100, InboundSource: "SOURCE-OLD",
		})
		testutil.MustNoErr(t, cerr, "upsert unchanged claim")
	}
	eng := testEngine(t, db)

	// A QUEUED order to the unchanged node: pre-dispatch, so the cancel would
	// take it if the two node sets were one variable.
	queued := seedOrderTo(t, db, "uuid-unchanged-queued", "SAME-NODE", false, orders.StatusQueued)

	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err != nil {
		t.Fatalf("changeover refused with only a queued order at an unchanged node: %v", err)
	}
	if got := reloadStatus(t, db, queued); got == orders.StatusCancelled {
		t.Error("a queued order at a SituationUnchanged participant was cancelled — the incoming " +
			"style still claims that payload there, so the order is still wanted")
	}
}
