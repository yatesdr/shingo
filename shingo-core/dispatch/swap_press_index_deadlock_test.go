//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// TestSwapHold_PressIndexR1_NotHeldOnItsSibling reproduces the press-index swap
// deadlock and pins it shut. It was live and permanent, and nothing on the
// two_robot path could see it.
//
// The old gate held any leg whose DeliveryNode != ProcessNode until its sibling
// had claimed a bin. Core's DeliveryNode is derived from the steps
// (extractEndpoints = last pickup-or-dropoff), so a press-index R1 — which ends
// by staging a fresh carrier at the index node — always looked like a removal leg
// that needed a sibling's help. It does not: R1 fetches that carrier itself.
//
// The sequence, all of which really happens:
//
//  1. R1 arrives and cannot claim (nothing in the supermarket here; at HK it was
//     a re-stamped payload on the press bin). It stays queued. It escaped the
//     gate only because its sibling pointer was still empty.
//  2. R2 arrives. Intake back-links BOTH rows (LinkSiblingsByEdgeUUID's
//     bidirectional CASE), so R1 now has a sibling. The trap is armed.
//  3. R1 re-evaluates: evac-shaped, sibling present, sibling holds no claim →
//     HELD, pending R2's claim.
//  4. R2's only source is the index position — empty, because filling it is R1's
//     job. R2 can never claim.
//
// R1 waits on R2; R2 waits on R1. The swap cannot bootstrap, on cold start or
// after any failed claim. The fix asks whether the leg SECURES ITS OWN
// REPLACEMENT (a second pickup, away from the line) rather than where it ends.
func TestSwapHold_PressIndexR1_NotHeldOnItsSibling(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, press, bp := setupTestData(t, db)

	market := &nodes.Node{Name: "PI-MARKET", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(market), "create market")
	indexB := &nodes.Node{Name: "PI-INDEX-B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(indexB), "create index B")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// R1 — clear the press, store the spent bin, fetch a fresh carrier, stage it
	// at the index. TWO pickups: it brings its own replacement into the swap.
	// No bins exist anywhere, so its claim fails and it stays queued (step 1).
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "pi-r1", PayloadCode: bp.Code, Quantity: 1, ProcessNode: press.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionWait, Node: press.Name},
			{Action: protocol.ActionPickup, Node: press.Name},
			{Action: protocol.ActionDropoff, Node: market.Name},
			{Action: protocol.ActionPickup, Node: market.Name},
			{Action: protocol.ActionDropoff, Node: indexB.Name},
		},
	})
	r1, err := db.GetOrderByUUID("pi-r1")
	testutil.MustNoErr(t, err, "get R1")
	if !protocol.IsAcquiring(r1.Status) {
		t.Fatalf("precondition: R1 status = %q, want queued/sourcing (it must fail its claim and stay retryable)", r1.Status)
	}
	if r1.BinID != nil {
		t.Fatalf("precondition: R1 claimed bin %d — the setup is meant to leave it unable to claim", *r1.BinID)
	}

	// R2 — index the staged bin onto the press. Carries the sibling pointer, so
	// intake back-links BOTH rows and arms the trap (step 2).
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "pi-r2", PayloadCode: bp.Code, Quantity: 1, ProcessNode: press.Name,
		SiblingOrderUUID: "pi-r1",
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionWait, Node: indexB.Name},
			{Action: protocol.ActionPickup, Node: indexB.Name},
			{Action: protocol.ActionDropoff, Node: press.Name},
		},
	})

	// The trap is armed: R1 now carries a sibling pointer it did not have when it
	// first dispatched. This is the state the old gate could not survive.
	r1, err = db.GetOrderByUUID("pi-r1")
	testutil.MustNoErr(t, err, "re-read R1")
	if r1.SiblingOrderUUID != "pi-r2" {
		t.Fatalf("precondition: R1 back-link = %q, want pi-r2 — intake must link both rows, or this test proves nothing", r1.SiblingOrderUUID)
	}

	// THE ASSERTION. R1 fetches its own replacement carrier, so it never depends
	// on R2's claim. Holding it here is the deadlock: R2's only pickup is the
	// index position that R1 has not filled yet.
	steps, ok := decodeSteps(r1.StepsJSON)
	if !ok {
		t.Fatal("R1 has no readable steps")
	}
	if held, reason := d.swapRemovalLegHeld(r1, steps); held {
		t.Fatalf("R1 held (%s) — it collects its own carrier (second pickup at the market) and can never be unblocked by R2, "+
			"whose only source is the index position R1 was going to fill. This is the deadlock.", reason)
	}

	// And the hold must not be the reason dispatch declines: R1 may still fail for
	// want of bins, but never with a swap hold.
	if derr := d.DispatchPreparedComplex(r1); derr != nil && strings.Contains(derr.Error(), "swap removal hold") {
		t.Fatalf("DispatchPreparedComplex declined R1 with a swap hold: %v", derr)
	}
}

// TestSwapHold_TwoRobotEvac_StillHeldUntilSupplyClaims is the other half of the
// contract: exempting press-index R1 must NOT weaken the ALN_003 guard. A
// two_robot evac has ONE pickup, at the line — it only removes, and cannot secure
// a replacement itself. It must still wait for its supply sibling to claim, or it
// pulls the line's bin with nothing coming (swap-starvation, 2026-06-03).
func TestSwapHold_TwoRobotEvac_StillHeldUntilSupplyClaims(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, line, bp := setupTestData(t, db)

	market := &nodes.Node{Name: "TR-MARKET", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(market), "create market")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Supply: fetch from the market, deliver to the line. Left queued (no bins).
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "tr-supply", PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: market.Name},
			{Action: protocol.ActionDropoff, Node: line.Name},
		},
	})

	// Evac: one pickup, at the line. Depends entirely on the supply.
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "tr-evac", PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		SiblingOrderUUID: "tr-supply",
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionWait, Node: line.Name},
			{Action: protocol.ActionPickup, Node: line.Name},
			{Action: protocol.ActionDropoff, Node: market.Name},
		},
	})

	evac, err := db.GetOrderByUUID("tr-evac")
	testutil.MustNoErr(t, err, "get evac")
	steps, ok := decodeSteps(evac.StepsJSON)
	if !ok {
		t.Fatal("evac has no readable steps")
	}
	if held, _ := d.swapRemovalLegHeld(evac, steps); !held {
		t.Fatal("two_robot evac must stay held while its supply holds no claim — it has a single pickup, at the line, " +
			"so it removes the line's bin with no replacement secured (ALN_003 swap-starvation)")
	}
}
