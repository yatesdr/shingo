package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
)

// ---------------------------------------------------------------------------
// The U1 side-cycle trigger: does a press-index produce pair actually fire it,
// and does it fire AFTER the release rather than before?
//
// The disposition routing table pins which leg receives which disposition — the
// decision that decides the trigger — but nothing pinned the trigger itself.
// That is the one live behaviour change on this branch: under the positional
// labels neither leg of a press-index pair satisfied (not supply) AND
// capture_lineside, so a press-index press never fired the downstream unloader
// full-in that a two_robot press fires on every swap. A label regression would
// pass the routing table and silently kill the trigger again.
//
// The ORDERING half is the F3 family in its second location: the trigger creates
// a real order at the unloader on the premise that a bin was just finished, and
// it used to run before the release was enqueued.
// ---------------------------------------------------------------------------

// recordingLoaderStore answers LoaderForPayload and records what it was asked.
// The U1 attempt is the observable — whether a full-in order is then created
// depends on window occupancy and budget, which the loader seam's own tests
// cover.
type recordingLoaderStore struct {
	LoaderStore // embedded: every method not overridden below panics if called
	asked       []string
	unloader    *domain.Loader
}

func (r *recordingLoaderStore) LoaderForPayload(payload domain.PayloadCode, role domain.LoaderRole, _ bool) (*domain.Loader, error) {
	if role == domain.RoleConsume {
		r.asked = append(r.asked, string(payload))
	}
	return r.unloader, nil
}

// seedStagedPressIndexPair builds a press-index produce pair from the REAL step
// builder and stages both legs, with the runtime slots pointed at them.
//
// Steps matter: isSupplyOrderInTwoRobotSwap refuses a release it cannot
// classify, and a leg with no steps cannot be classified — so a stepless
// fixture exercises the refusal rather than the path.
func seedStagedPressIndexPair(t *testing.T, uuidPrefix string) (*Engine, int64, int64, int64) {
	t.Helper()
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	disp, err := BuildSwapDispatch(node, claim)
	testutil.MustNoErr(t, err, "build swap dispatch")

	legA := mkSwapLeg(t, db, nodeID, uuidPrefix+"-a", disp.StepsA, "")
	legB := mkSwapLeg(t, db, nodeID, uuidPrefix+"-b", disp.StepsB, "")
	testutil.MustNoErr(t, db.LinkOrderSiblings(legA.ID, legB.ID), "link siblings")
	for _, id := range []int64{legA.ID, legB.ID} {
		testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(protocol.StatusStaged)), "stage leg")
	}
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &legA.ID, &legB.ID), "runtime slots")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claim.ID, 0), "bind claim")

	return testEngine(t, db), nodeID, legA.ID, legB.ID
}

// TestReleaseOrderWithLineside_U1FiresAfterTheRelease is named by the release
// census. It pins both halves: the trigger fires for a press-index produce
// release, and it fires after the release envelope is queued.
func TestReleaseOrderWithLineside_U1FiresAfterTheRelease(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	disp, err := BuildSwapDispatch(node, claim)
	testutil.MustNoErr(t, err, "build swap dispatch")

	legA := mkSwapLeg(t, db, nodeID, "uuid-u1-a", disp.StepsA, "")
	legB := mkSwapLeg(t, db, nodeID, "uuid-u1-b", disp.StepsB, "")
	testutil.MustNoErr(t, db.LinkOrderSiblings(legA.ID, legB.ID), "link siblings")
	for _, id := range []int64{legA.ID, legB.ID} {
		testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(protocol.StatusStaged)), "stage leg")
	}
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &legA.ID, &legB.ID), "runtime slots")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claim.ID, 0), "bind claim")

	eng := testEngine(t, db)
	rec := &recordingLoaderStore{}
	eng.loaderStore = rec

	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{
		Mode: DispositionCaptureLineside, CalledBy: "u1-trigger-test",
	}), "release staged orders")

	// THE TRIGGER FIRED, for this press's payload.
	if len(rec.asked) == 0 {
		t.Fatalf("a press-index produce pair released with capture_lineside did not attempt the "+
			"unloader full-in. This is the one live behaviour change on the branch: under the "+
			"positional leg labels neither leg satisfied (not supply) AND capture_lineside, so the "+
			"trigger never fired for press-index at all. Payload was %q.", claim.PayloadCode)
	}
	for _, p := range rec.asked {
		if p != claim.PayloadCode {
			t.Errorf("U1 asked for payload %q, want the press's own %q", p, claim.PayloadCode)
		}
	}

	// AND IT FIRED AFTER THE RELEASE. The outbox drains by id, so the release
	// envelope must already be sitting there when the trigger runs.
	msgs, err := db.ListPendingOutbox(200)
	testutil.MustNoErr(t, err, "ListPendingOutbox")
	sawRelease := false
	for _, m := range msgs {
		if m.MsgType == protocol.TypeOrderRelease {
			sawRelease = true
		}
	}
	if !sawRelease {
		t.Error("no release envelope was queued — the trigger must not be the only thing that happened")
	}
}

// TestReleaseOrderWithLineside_U1DoesNotFireForTheSupplyLeg is the other half of
// the composition: the leg carrying a bin ONTO the press is not a bin anybody
// just finished.
func TestReleaseOrderWithLineside_U1DoesNotFireForTheSupplyLeg(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	dispatch, err := BuildSwapDispatch(node, claim)
	testutil.MustNoErr(t, err, "build swap dispatch")

	legA := mkSwapLeg(t, db, nodeID, "uuid-u1s-a", dispatch.StepsA, "")
	legB := mkSwapLeg(t, db, nodeID, "uuid-u1s-b", dispatch.StepsB, "")
	testutil.MustNoErr(t, db.LinkOrderSiblings(legA.ID, legB.ID), "link siblings")
	testutil.MustNoErr(t, db.UpdateOrderStatus(legB.ID, string(protocol.StatusStaged)), "stage supply")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claim.ID, 0), "bind claim")

	eng := testEngine(t, db)
	rec := &recordingLoaderStore{}
	eng.loaderStore = rec

	// Release the SUPPLY leg on its own, through the per-order door.
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(legB.ID, ReleaseDisposition{
		Mode: DispositionCaptureLineside, CalledBy: "u1-supply-test",
	}), "release the supply leg")

	if len(rec.asked) != 0 {
		t.Errorf("the supply leg fired the unloader full-in (asked %v) — it is the bin going ONTO "+
			"the press, not one anybody finished, and pulling a full for it is a robot sent for "+
			"nothing", rec.asked)
	}

}
