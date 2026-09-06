package engine

import (
	"strings"
	"testing"

	"shingoedge/orders"
)

// The line-pull guard was moved onto the trunk because a third door bypassed
// it. Core's own release precondition had the same shape and was never moved:
// the changeover doors checked it and the per-order door — /orders/{id}/release,
// straight into ReleaseOrderWithLineside — did not.
//
// Without the check, Manager.ReleaseOrderWithDisposition forces the row to
// in_transit anyway, so Edge records a release Core never accepted and the two
// disagree about an order nobody is moving. The operator gets a success they
// do not have.
func TestReleaseOrderWithLineside_RefusesWhatCoreWillNotTake(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "REL-PRE", PayloadCode: "PART-RP", UOPCapacity: 100, InitialUOP: 50,
	})
	bin := int64(8100)
	if err := db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, 50); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	orderID, err := db.CreateOrder("uuid-rel-pre", orders.TypeRetrieve,
		&nodeID, false, 1, "REL-PRE-NODE", "", "", "", false, "PART-RP")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	// Left at its created status — not staged, not in_transit.
	eng := testEngine(t, db)

	err = eng.ReleaseOrderWithLineside(orderID, ReleaseDisposition{Mode: DispositionCaptureLineside})
	if err == nil {
		t.Fatal("releasing a non-releasable order reported success. Core refuses anything that " +
			"is not staged or in_transit, so this would leave Edge recording a release that " +
			"never happened.")
	}
	if !strings.Contains(err.Error(), "Core will not release") {
		t.Errorf("refusal does not say what refused it: %v", err)
	}
}

// "Not releasable" tells an operator nothing they can act on. When Core has
// mirrored a queue reason onto the order, the refusal names it.
func TestReleaseOrderWithLineside_RefusalNamesCoresBlocker(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "REL-WHY", PayloadCode: "PART-RW", UOPCapacity: 100, InitialUOP: 50,
	})
	bin := int64(8101)
	if err := db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, 50); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	orderID, err := db.CreateOrder("uuid-rel-why", orders.TypeRetrieve,
		&nodeID, false, 1, "REL-WHY-NODE", "", "", "", false, "PART-RW")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.SetOrderQueueReason("uuid-rel-why", "no empty slot at SMN_029", "no_slot"); err != nil {
		t.Fatalf("mirror queue reason: %v", err)
	}
	eng := testEngine(t, db)

	err = eng.ReleaseOrderWithLineside(orderID, ReleaseDisposition{Mode: DispositionCaptureLineside})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"no empty slot at SMN_029", "no_slot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not carry %q — the operator needs to know what to go fix, not "+
				"that a predicate returned false.\nGot: %v", want, err)
		}
	}
}

// A staged order still releases. The check is a precondition, not a new door.
func TestReleaseOrderWithLineside_StagedStillReleases(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "REL-OK", PayloadCode: "PART-RO", UOPCapacity: 100, InitialUOP: 50,
	})
	bin := int64(8102)
	if err := db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, 50); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	orderID, err := db.CreateOrder("uuid-rel-ok", orders.TypeRetrieve,
		&nodeID, false, 1, "REL-OK-NODE", "", "", "", false, "PART-RO")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, "staged"); err != nil {
		t.Fatalf("stage order: %v", err)
	}
	eng := testEngine(t, db)

	if err := eng.ReleaseOrderWithLineside(orderID, ReleaseDisposition{Mode: DispositionCaptureLineside}); err != nil {
		t.Fatalf("a staged order must still release: %v", err)
	}
}
