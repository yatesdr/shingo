package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
)

// produce_release_ingest_test.go — Fix D: the produce manifest snapshots at
// the RELEASE tap, not the REQUEST tap. Every part pressed while the robots
// travel still lands in the departing bin, so the request-time snapshot
// understated the shipped tote and pre-credited the next one.

// listIngests decodes every TypeOrderIngest in the outbox, keeping outbox ids
// so ordering against the release envelopes can be asserted.
func listIngests(t *testing.T, db *store.DB) (reqs []protocol.OrderIngestRequest, ids []int64) {
	t.Helper()
	msgs, err := db.ListPendingOutbox(100)
	testutil.MustNoErr(t, err, "list outbox")
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderIngest {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "unmarshal envelope")
		var req protocol.OrderIngestRequest
		testutil.MustNoErr(t, env.DecodePayload(&req), "decode ingest")
		reqs = append(reqs, req)
		ids = append(ids, m.ID)
	}
	return reqs, ids
}

// TestProduceTwoRobot_RequestDefersPaperwork: the REQUEST tap on a two-robot
// produce node dispatches the legs but stamps NO manifest and keeps the count
// ticking — both halves of the deferred paperwork.
func TestProduceTwoRobot_RequestDefersPaperwork(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	if _, err := eng.RequestProduceSwap(nodeID); err != nil {
		t.Fatalf("RequestProduceSwap: %v", err)
	}

	if ingests, _ := listIngests(t, db); len(ingests) != 0 {
		t.Fatalf("ingest stamps at request = %d, want 0 — the manifest locks at release", len(ingests))
	}
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOPCached != 50 {
		t.Errorf("RemainingUOP = %d, want 50 still ticking on the departing bin", runtime.RemainingUOPCached)
	}
}

// TestReleaseStagedOrders_IngestAtRelease is the disposition itself: the
// release-time manifest carries the LIVE count (parts pressed during robot
// transit included), pins the departing bin by id, lands in the outbox BEFORE
// both release envelopes (Core must apply the manifest first — the outbox
// drains strictly by id), and only then does the count reset.
func TestReleaseStagedOrders_IngestAtRelease(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	result, err := eng.RequestProduceSwap(nodeID)
	testutil.MustNoErr(t, err, "RequestProduceSwap")
	markStaged(t, db, result.OrderA.ID)
	markStaged(t, db, result.OrderB.ID)

	// Press keeps running during robot transit: 50 → 61. Bind the departing
	// bin so the manifest can pin it (Core seeded this id at delivery in
	// production; the seed here is direct).
	cID := claimID
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &cID, 61), "bump count")
	departing := int64(777)
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &departing), "bind departing bin")

	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{CalledBy: "test-op"}), "release")

	ingests, ingestIDs := listIngests(t, db)
	if len(ingests) != 1 {
		t.Fatalf("release-time ingest stamps = %d, want exactly 1", len(ingests))
	}
	if ingests[0].Quantity != 61 {
		t.Errorf("manifest quantity = %d, want 61 — the LIVE count at release, not the request-time 50", ingests[0].Quantity)
	}
	if ingests[0].BinID != departing {
		t.Errorf("manifest bin id = %d, want %d — resolve-by-node can hit the freshly indexed tote", ingests[0].BinID, departing)
	}
	if len(ingests[0].Manifest) != 1 || ingests[0].Manifest[0].PartNumber != "WIDGET-A" {
		t.Errorf("manifest items = %+v, want one WIDGET-A entry", ingests[0].Manifest)
	}

	// Ordering: the ingest must precede BOTH OrderRelease envelopes.
	msgs, err := db.ListPendingOutbox(100)
	testutil.MustNoErr(t, err, "list outbox for ordering")
	releases := 0
	for _, m := range msgs {
		if m.MsgType == protocol.TypeOrderRelease {
			releases++
			if m.ID < ingestIDs[0] {
				t.Errorf("OrderRelease outbox id %d precedes the ingest id %d — Core would apply the release-side manifest action first", m.ID, ingestIDs[0])
			}
		}
	}
	if releases != 2 {
		t.Errorf("OrderRelease envelopes = %d, want 2 (both legs)", releases)
	}

	// The paperwork's second half: count reset AFTER the snapshot, so the
	// hold-and-replay window starts at release.
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOP = %d, want 0 after release", runtime.RemainingUOPCached)
	}
	if runtime.ActiveBinID != nil {
		t.Errorf("ActiveBinID = %v, want cleared (ticks hold until the next bin binds)", runtime.ActiveBinID)
	}
}

// TestReleaseStagedOrders_RetrySkipsSecondIngest: the count-zero guard makes
// the release click idempotent — a retry re-fires the release envelopes but
// never a second manifest.
func TestReleaseStagedOrders_RetrySkipsSecondIngest(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "two_robot")
	eng := testEngine(t, db)

	result, err := eng.RequestProduceSwap(nodeID)
	testutil.MustNoErr(t, err, "RequestProduceSwap")
	markStaged(t, db, result.OrderA.ID)
	markStaged(t, db, result.OrderB.ID)

	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{}), "first release")
	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{}), "retry release")

	if ingests, _ := listIngests(t, db); len(ingests) != 1 {
		t.Fatalf("ingest stamps after retry = %d, want exactly 1 (zero-count guard)", len(ingests))
	}
}
