package engine

import (
	"encoding/json"
	"sync"
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

	if _, err := eng.RequestProduceSwap(nodeID, 0); err != nil {
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

	result, err := eng.RequestProduceSwap(nodeID, 0)
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
	// No lines: Core resolves the payload's template and stamps this count.
	if len(ingests[0].Manifest) != 0 || ingests[0].PayloadCode != "WIDGET-A" {
		t.Errorf("ingest payload %q, lines %+v; want WIDGET-A and no lines", ingests[0].PayloadCode, ingests[0].Manifest)
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

	result, err := eng.RequestProduceSwap(nodeID, 0)
	testutil.MustNoErr(t, err, "RequestProduceSwap")
	markStaged(t, db, result.OrderA.ID)
	markStaged(t, db, result.OrderB.ID)

	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{}), "first release")
	testutil.MustNoErr(t, eng.ReleaseStagedOrders(nodeID, ReleaseDisposition{}), "retry release")

	if ingests, _ := listIngests(t, db); len(ingests) != 1 {
		t.Fatalf("ingest stamps after retry = %d, want exactly 1 (zero-count guard)", len(ingests))
	}
}

// TestCharSP_FinalizeShipsTheCountAtTheFinalize — strict-pack step 1's pin 3,
// flipped by P3. Driven through the real door (door 1, the HMI's bare body, on
// a two_robot produce pair): the act loads the pair's runtime (PlanPair,
// NeedActive), then asks Core's release points (gatesOnThePoint), then commits.
// The fake Core's points answer is where a tick is interposed, so it lands
// after the act's read and before the finalize.
//
// At the base the finalize shipped the act's read (42) and the clear zeroed the
// seat the tick had charged (45): the 3 were in no ingest and not held, and at
// Core the +3 delta drained before the ingest set the bin to 42. Now the
// finalize reads the seat under countMu through the flush, the ingest and the
// clear: the ingest ships 45, and a tick after the clear is held for the next
// bin (the supply leg is placing one), never charged to the departed bin.
func TestCharSP_FinalizeShipsTheCountAtTheFinalize(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	pairAt(pairSpec{mode: protocol.SwapModeTwoRobot}, "evac", protocol.StatusStaged, "supply", protocol.StatusStaged)(h)
	styleID := activeStyleOf(t, h.eng, h.processID)
	const tick = 3
	var once sync.Once
	ticked := false
	h.core.mu.Lock()
	h.core.points = func(req protocol.ReleasePointsRequest) protocol.ReleasePointsResponse {
		once.Do(func() {
			charSPTick(h.eng, h.processID, styleID, tick)
			ticked = true
		})
		return h.fakePoints(req)
	}
	h.core.mu.Unlock()
	h.mark()

	testutil.MustNoErr(t, h.eng.ReleaseStagedOrders(h.nodeID, dispNone), "pair click")
	if !ticked {
		t.Fatal("fixture: the act never asked Core's points, so no tick was interposed")
	}

	h.mutator.Flush()
	var ingests []protocol.OrderIngestRequest
	tickDeltaBeforeIngest := false
	for _, m := range h.sinceMark() {
		switch m.MsgType {
		case protocol.TypeOrderIngest:
			var env protocol.Envelope
			testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "unmarshal ingest envelope")
			var req protocol.OrderIngestRequest
			testutil.MustNoErr(t, env.DecodePayload(&req), "decode ingest")
			ingests = append(ingests, req)
		case protocol.SubjectBinUOPDelta:
			d := decodeBinUOPDelta(t, m.Payload)
			if d.Reason == protocol.ReasonProduceTick && d.BinID == fxBin && d.Delta == tick && len(ingests) == 0 {
				tickDeltaBeforeIngest = true
			}
		}
	}
	if len(ingests) != 1 {
		t.Fatalf("ingests = %d, want 1", len(ingests))
	}
	if ingests[0].Quantity != fxCount+tick {
		t.Errorf("ingest quantity = %d, want %d: the finalize ships the seat as it stands at the finalize, the tick included",
			ingests[0].Quantity, fxCount+tick)
	}
	if !tickDeltaBeforeIngest {
		t.Errorf("want the interposed tick's produce_tick (+%d on bin %d) queued before the ingest", tick, fxBin)
	}
	rt := runtimeOf(t, h.eng, h.nodeID)
	if rt.RemainingUOPCached != 0 || rt.PendingUOPDelta != 0 || rt.ActiveBinID != nil {
		t.Errorf("seat after the release = bin %v count %d hold %d, want unbound, 0, 0",
			rt.ActiveBinID, rt.RemainingUOPCached, rt.PendingUOPDelta)
	}

	// A tick after the clear is the next bin's: held, not charged.
	h.mark()
	charSPTick(h.eng, h.processID, styleID, 2)
	h.mutator.Flush()
	rt = runtimeOf(t, h.eng, h.nodeID)
	if rt.PendingUOPDelta != 2 || rt.PendingUOPPayload != fxPart || rt.ActiveBinID != nil {
		t.Errorf("seat after a later tick = bin %v hold %d of %q, want unbound holding 2 of %s",
			rt.ActiveBinID, rt.PendingUOPDelta, rt.PendingUOPPayload, fxPart)
	}
	for _, m := range h.sinceMark() {
		if m.MsgType != protocol.SubjectBinUOPDelta {
			continue
		}
		if d := decodeBinUOPDelta(t, m.Payload); d.BinID == fxBin {
			t.Errorf("a tick after the release charged the departed bin: %+v", d)
		}
	}
}

// TestFinalize_ATickRacingTheReleaseIsCountedOnce: a tick that arrives while
// the release is being committed is in the departing bin's ingest or in the
// hold for the next bin, never in neither. The tick runs on its own goroutine,
// started at Core's points answer (after the act's read), so it lands before
// the finalize takes countMu (in the ingest) or waits for it and lands after
// the clear (in the hold). At the base it usually landed before the finalize
// and was lost: the ingest shipped the act's read and the clear zeroed it.
func TestFinalize_ATickRacingTheReleaseIsCountedOnce(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	pairAt(pairSpec{mode: protocol.SwapModeTwoRobot}, "evac", protocol.StatusStaged, "supply", protocol.StatusStaged)(h)
	styleID := activeStyleOf(t, h.eng, h.processID)
	const tick = 3
	var once sync.Once
	done := make(chan struct{})
	h.core.mu.Lock()
	h.core.points = func(req protocol.ReleasePointsRequest) protocol.ReleasePointsResponse {
		once.Do(func() {
			go func() {
				charSPTick(h.eng, h.processID, styleID, tick)
				close(done)
			}()
		})
		return h.fakePoints(req)
	}
	h.core.mu.Unlock()
	h.mark()

	testutil.MustNoErr(t, h.eng.ReleaseStagedOrders(h.nodeID, dispNone), "pair click")
	<-done

	ingests, _ := listIngests(t, h.db)
	if len(ingests) != 1 {
		t.Fatalf("ingests = %d, want 1", len(ingests))
	}
	rt := runtimeOf(t, h.eng, h.nodeID)
	if got := int(ingests[0].Quantity) + int(rt.PendingUOPDelta); got != fxCount+tick || rt.ActiveBinID != nil || rt.RemainingUOPCached != 0 {
		t.Errorf("ingest %d + hold %d = %d (seat bin %v count %d), want %d: every stroke in the ingest or the hold, the seat unbound",
			ingests[0].Quantity, rt.PendingUOPDelta, got, rt.ActiveBinID, rt.RemainingUOPCached, fxCount+tick)
	}
}

// decodeBinUOPDelta reads a BinUOPDelta data envelope from the outbox.
func decodeBinUOPDelta(t *testing.T, payload []byte) protocol.BinUOPDelta {
	t.Helper()
	var env struct {
		P struct {
			Data protocol.BinUOPDelta `json:"data"`
		} `json:"p"`
	}
	testutil.MustNoErr(t, json.Unmarshal(payload, &env), "decode BinUOPDelta envelope")
	return env.P.Data
}
