package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"shingo/protocol/testutil"
)

// TestRegression_ChangeoverCancelReconcilesActiveBinFromPhysical pins the
// Springfield 2026-06-02 cancel-path fix. Cancelling a changeover used to clear
// only the runtime ORDER refs and leave active_bin_id pointing at the old bin —
// so after an evac moved (or an operator manually swapped) the bin, consume
// ticks kept draining the stale bin. The cancel now re-resolves each affected
// node against Core's physical bin-at-node and rebinds the pointer (with the
// authoritative count + epoch).
func TestRegression_ChangeoverCancelReconcilesActiveBinFromPhysical(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)

	node, _ := db.GetProcessNode(nodeID)
	coreNode := node.CoreNodeName

	// Core reports a DIFFERENT bin physically at the slot than the stale
	// active pointer — the re-resolved / manually-swapped bin.
	const physicalBinID int64 = 555
	const physicalUOP = 77
	const physicalEpoch int64 = 9
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]NodeBinInfo{{
			NodeName:     coreNode,
			BinID:        physicalBinID,
			UOPRemaining: physicalUOP,
			DeltaEpoch:   physicalEpoch,
			Occupied:     true,
		}})
	}))
	defer srv.Close()

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.coreClient = NewCoreClient(srv.URL)

	_, _ = startChangeover(t, eng, db, processID, toStyleID)

	// Stamp a STALE active bin (the old bin the evac was removing, left
	// dangling). Thread the existing claim through unchanged.
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	staleBin := int64(999)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, rt.ActiveClaimID, &staleBin, 5), "seed stale active bin")

	testutil.MustNoErr(t, eng.CancelProcessChangeover(processID), "cancel")

	post, _ := db.GetProcessNodeRuntime(nodeID)
	if post.ActiveBinID == nil || *post.ActiveBinID != physicalBinID {
		t.Errorf("post-cancel ActiveBinID = %v, want %d (must rebind to the physical bin, not the stale 999)",
			post.ActiveBinID, physicalBinID)
	}
	if post.RemainingUOPCached != physicalUOP {
		t.Errorf("post-cancel RemainingUOPCached = %d, want %d (must reconcile to Core's authoritative count)",
			post.RemainingUOPCached, physicalUOP)
	}
}
