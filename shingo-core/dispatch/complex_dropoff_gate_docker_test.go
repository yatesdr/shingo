//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// ── THE DROPOFF-CAPACITY GATE, END TO END ─────────────────────────────────
//
// reserveComplexDestination is Phase C of DispatchPreparedComplex and it is the
// arm that was once widened to cover loader homes. That widening reopened a
// named Springfield incident and was reverted on 2026-08-31 (WALL) without ever
// reaching origin — dispatch/loader_place.go carries the full argument at the
// place it matters. The next person who wants
// to widen this predicate should find a harness here rather than discovering,
// as we did, that the suite stays green either way.
//
// WHAT WAS ALREADY PINNED, so this test says what it adds.
// TestDispatchPreparedComplex_QueuesOnFullConcreteStorageDropoff (bin_lifecycle_test.go)
// does drive this phase and asserts the order queues. What it does not ask is
// anything that distinguishes one refusal from another:
//
//	it asserts QueueReason != ""       — any cause passes, including
//	                                     capacity-check-failed from a read error
//	it asserts the order stays queued  — but never that it stops being queued
//
// The second gap is the one that matters. A gate that queues an order and a gate
// that queues it FOREVER look identical to that test, and "queues forever" is
// exactly what the reverted commit did to a loader home: the queued leg went
// invisible to the replenishment loop's in-flight yield check
// (status != 'queued'), the loop refilled the home, and the gate refused on the
// carrier it had caused to be put there. A release arm is the only thing that
// tells those two apart.
//
// So this adds the two halves that discriminate: the CAUSE is dropoff-occupied
// specifically and carries the count an operator acts on, and the SAME order
// dispatches once the slot frees, with nothing else changed.

// occupiedStorageDropoff builds a complex order whose final dropoff is a
// concrete storage slot under an NGRP with a bin standing on it, plus the line
// bin its pickup step needs.
//
// Concrete-child-of-NGRP is deliberate: isConcreteStorageDropoff is a ROLE test,
// and a LINE dropoff must classify false or a two-robot supply leg gets held on
// the node its own sibling evac is coming to clear — the 2b05dce deadlock. This
// fixture is on the storage side of that line, which is the side that gates.
func occupiedStorageDropoff(t *testing.T, db *store.DB, d *Dispatcher, lineNode *nodes.Node, bp *payloads.Payload) (slot *nodes.Node, occupant *bins.Bin, orderUUID string) {
	t.Helper()

	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP node type")
	grp := &nodes.Node{Name: "DGATE-NGRP", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create NGRP")

	slot = &nodes.Node{Name: "DGATE-NGRP-S1", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(slot), "create storage slot")

	occupant = &bins.Bin{BinTypeID: 1, Label: "DGATE-OCCUPANT", NodeID: &slot.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(occupant), "occupy the slot")

	// The bin the pickup step will claim, manifested and confirmed so the source
	// side of the dispatch can actually succeed — otherwise a later phase refuses
	// for its own reasons and the release arm proves nothing.
	lineBin := &bins.Bin{BinTypeID: 1, Label: "DGATE-LINEBIN", NodeID: &lineNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(lineBin), "create line bin")
	testutil.MustNoErr(t, db.SetBinManifest(lineBin.ID, `{"items":[{"catid":"PART-A","qty":100}]}`, bp.Code, 100), "set manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(lineBin.ID, ""), "confirm manifest")

	orderUUID = "uuid-dgate-occupied"
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID:   orderUUID,
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "wait", Node: lineNode.Name},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: slot.Name},
		},
	})
	return slot, occupant, orderUUID
}

// TestDispatchPreparedComplex_DropoffOccupiedQueuesThenReleases is the harness
// the reverted loader-home widening did not have.
//
// Two arms, and the second is the point: the gate is a WAIT, not a refusal, and
// only a run that ends with the order dispatching proves the wait has a releaser.
func TestDispatchPreparedComplex_DropoffOccupiedQueuesThenReleases(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	slot, occupant, orderUUID := occupiedStorageDropoff(t, db, d, lineNode, bp)

	order, err := db.GetOrderByUUID(orderUUID)
	testutil.MustNoErr(t, err, "get order")

	// ── ARM 1: a bin is standing on the destination, so this waits ──────────
	if derr := d.DispatchPreparedComplex(order); derr == nil {
		t.Fatal("dispatched onto an occupied concrete storage slot — a robot sent there arrives at a " +
			"slot it cannot lower into and stands in the aisle holding the bin")
	}

	got, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "re-read order")
	if got.Status != StatusQueued {
		t.Errorf("status = %q, want queued", got.Status)
	}
	if got.VendorOrderID != "" {
		t.Errorf("VendorOrderID = %q, want empty — nothing should have reached the fleet", got.VendorOrderID)
	}

	// THE CAUSE, NOT MERELY "SOME CAUSE". dropoff-occupied and
	// capacity-check-failed both queue the order and both leave QueueReason
	// non-empty, and they send an operator to two different places: go and clear
	// the slot, versus Core could not read the slot at all. A test that accepts
	// either cannot tell a working gate from a gate that is failing closed on a
	// broken read.
	if QueueCause(got.QueueCause) != CauseDropoffOccupied {
		t.Errorf("queue cause = %q, want %q — a bin is physically standing on the slot, which is the "+
			"one cause that tells an operator to go and move it", got.QueueCause, CauseDropoffOccupied)
	}

	// AND THE COUNT REACHES THE SENTENCE. CheckDropoffCapacity carries
	// BlockingBins into the params precisely so "a bin is sitting there" and "an
	// order is on its way" stop rendering as the same bare "Waiting for a slot".
	// If the params are dropped the discriminator exists only in a column no
	// surface renders.
	if !strings.Contains(got.QueueReason, "1 bin there now") {
		t.Errorf("queue reason = %q, want it to name the occupancy count (\"1 bin there now\") — "+
			"the count is the discriminator between go-clear-it and wait", got.QueueReason)
	}

	// ── ARM 2: the occupant leaves, and the SAME order goes ────────────────
	//
	// Retiring nulls node_id, which is what the gate reads (CountBinsByNode
	// filters retired). It stands in for the carrier being taken away; the gate
	// has no opinion about how the slot emptied, only that it did.
	testutil.MustNoErr(t, db.RetireBin(occupant.ID), "retire the occupant")

	if n, cErr := db.CountBinsByNode(slot.ID); cErr != nil || n != 0 {
		t.Fatalf("fixture: slot %s still reads %d bin(s) (err %v) — arm 2 would be testing nothing",
			slot.Name, n, cErr)
	}

	order, err = db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "re-read order before retry")
	if derr := d.DispatchPreparedComplex(order); derr != nil {
		t.Fatalf("still refused after the slot freed: %v — the gate is a WAIT and its releaser is the "+
			"occupant leaving. A gate with no releaser is the defect that reverted the "+
			"loader-home widening: the leg "+
			"sits queued, goes invisible to every in-flight count (status != 'queued'), and nothing "+
			"downstream can tell it apart from an order nobody raised", derr)
	}

	final, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "re-read order after retry")
	if final.VendorOrderID == "" {
		t.Errorf("VendorOrderID empty after the slot freed — the order never reached the fleet")
	}
	if final.Status == StatusQueued {
		t.Errorf("status still %q after the slot freed", final.Status)
	}
	if QueueCause(final.QueueCause) == CauseDropoffOccupied {
		t.Errorf("queue cause is still %q on a dispatched order — a stale cause outlives its "+
			"condition and sends an operator to a slot nobody is waiting on", final.QueueCause)
	}
}
