package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
)

// The U1 side-cycle trigger asked for a full of the claim's payload. The claim
// it used comes from resolveReleaseClaim, which returns the TARGET style's
// claim whenever any changeover is open on the process — so mid-changeover this
// ordered the INCOMING part into an unloader still draining the outgoing one,
// or resolved no unloader at all and dropped the U1 in silence.
//
// The right value was in scope: the bin being finished is the order's bin, and
// its payload is what the order recorded at create time. This same file states
// that rule a hundred lines below the site that broke it.
func TestU1_AsksForTheFinishedBinsPayloadNotTheClaims(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, claimID := seedProduceNode(t, db, protocol.SwapModeSimple)
	_ = processID

	// The order carries the OUTGOING part. The node's claim names WIDGET-A.
	const finished = "PART-BEING-FINISHED"
	orderID, err := db.CreateOrder("uuid-u1-payload", orders.TypeRetrieve, &nodeID, false, 1,
		"PRODUCE-NODE", "", "", "", false, finished)
	testutil.MustNoErr(t, err, "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusStaged)), "stage order")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil), "point runtime at the order")
	cID := claimID
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &cID, 0), "seat the claim")

	eng := testEngine(t, db)
	rec := &recordingLoaderStore{}
	eng.loaderStore = rec

	testutil.MustNoErr(t,
		eng.ReleaseOrderWithLineside(orderID, ReleaseDisposition{Mode: DispositionCaptureLineside}),
		"release")

	if len(rec.asked) == 0 {
		t.Fatal("no U1 lookup happened at all — the produce capture_lineside branch is the only " +
			"way this trigger fires, so the fixture is not exercising it")
	}
	for _, got := range rec.asked {
		if got == "WIDGET-A" {
			t.Fatalf("the U1 asked for the CLAIM's payload %q. The bin being finished is the "+
				"order's bin, carrying %q; asking for the claim's part orders the wrong full "+
				"into the unloader, or resolves no unloader and drops the U1 silently.",
				got, finished)
		}
	}
	if rec.asked[0] != finished {
		t.Errorf("U1 asked for %q, want the finished bin's payload %q", rec.asked[0], finished)
	}
}
