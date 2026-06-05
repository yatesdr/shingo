//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// =============================================================================
// UOP regression tests — fix-revealing
//
// Every test in this file is t.Skip'd with a "remove on fix #N" marker.
// They encode the post-fix contract for the bugs identified in the
// 2026-05 UOP deep-walk:
//
//   - #11 handleNormalReplenishment predicate: only fire when an order's
//     LAST DROPOFF equals the process node. Today it fires whenever the
//     order's process_node matches, including orders that REMOVE the bin
//     from the line (Order B in two-robot consume, R1 in press-index,
//     sequential-removal step). That spuriously resets line UOP to capacity
//     while the line is still draining the previous bin.
//
//   - #15 SyncOrClearForReleased manifest reconstruction: on RELEASE PARTIAL
//     (positive remaining_uop), Core must rewrite the bin's manifest JSON
//     to reflect the new UOP so the bin record at storage matches the
//     line's view. Today only uop_remaining is updated; the manifest stays
//     stale (carrying the pre-consumption qty). Per the single-payload
//     normalization assumption, the reconstructed manifest is:
//         {"items":[{"catid": payload_code, "qty": remaining_uop}]}
//
// Each test below FAILS on current code and PASSES once its named fix lands.
// When you land the fix:
//   1. Remove the t.Skip line in this file.
//   2. Confirm the test now passes against your fix.
//   3. Commit test removal-of-skip + fix together.
// =============================================================================

// NOTE: The #11 regression test lives in shingo-edge/engine/
// uop_regression_test.go (TestRegression_11_RemovalOrderDoesNotResetLineUOP).
// handleNormalReplenishment is an Edge-side function; testing the
// predicate flip from Core would require the full Edge↔Core harness
// (see integration/harness/). The Edge-local test runs against a
// SQLite test DB and exercises the predicate directly.

// TestRegression_15_PartialBackReconstructsManifest is the strengthened
// version of TestHandleOrderRelease_RemainingUOPPositiveSyncsUOP. The
// existing test asserts `got.Manifest == nil` is false but never reads
// the content. Pre-fix: manifest carries the pre-release qty (e.g. 100).
// Post-fix: manifest carries the released qty (e.g. 800), reconstructed
// from payload_code + remaining_uop per the single-payload normalization
// assumption.
//
// This is the assertion that would have caught the SMN_003 + ALN_002
// stale-manifest incidents the late-bind manifest sync was supposed to
// close — the existing test was too weak to fail on the real bug.
func TestRegression_15_PartialBackReconstructsManifest(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	_, bin := stageComplexOrderWithLineBin(t, db, d, lineNode, bp, "uuid-reg15-pos", "BIN-REG15-POS")

	partial := 800
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID:    "uuid-reg15-pos",
		RemainingUOP: &partial,
	})

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 800 {
		t.Errorf("UOPRemaining = %d, want 800", got.UOPRemaining)
	}
	if got.PayloadCode != bp.Code {
		t.Errorf("PayloadCode = %q, want %q", got.PayloadCode, bp.Code)
	}
	if got.Manifest == nil {
		t.Fatal("Manifest = nil; want reconstructed single-payload manifest")
	}

	// Strengthened assertion: parse the manifest and check item shape.
	parsed, err := got.ParseManifest()
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(parsed.Items) != 1 {
		t.Fatalf("manifest items = %d, want 1 (single-payload normalization)", len(parsed.Items))
	}
	item := parsed.Items[0]
	if item.CatID != bp.Code {
		t.Errorf("manifest item CatID = %q, want %q (= payload_code per single-payload normalization)",
			item.CatID, bp.Code)
	}
	if item.Quantity != int64(partial) {
		t.Errorf("manifest item Quantity = %d, want %d (= remaining_uop)",
			item.Quantity, partial)
	}
}

// TestRegression_15_PartialBackFallbackReconstructsManifest is the source-
// node fallback variant of #15. When claimComplexBins missed populating
// order.BinID, HandleOrderRelease locates the bin by source-node lookup
// and calls SyncOrClearForReleasedNoOwner. The reconstruction must apply
// on the fallback path too — otherwise we patch one path and leave the
// safety-net path stale.
func TestRegression_15_PartialBackFallbackReconstructsManifest(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	bin := &bins.Bin{BinTypeID: 1, Label: "BIN-REG15-FB-PART", NodeID: &lineNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")
	preManifest := `{"items":[{"catid":"` + bp.Code + `","qty":100}]}`
	testutil.MustNoErr(t, db.SetBinManifest(bin.ID, preManifest, bp.Code, 100), "set manifest")

	order := &orders.Order{
		EdgeUUID:     "uuid-reg15-fb-partial",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusStaged,
		Quantity:     1,
		SourceNode:   lineNode.Name,
		DeliveryNode: "OUTBOUND-DEST",
		PayloadCode:  bp.Code,
		StepsJSON:    `[{"action":"wait","node":"` + lineNode.Name + `"},{"action":"pickup","node":"` + lineNode.Name + `"},{"action":"dropoff","node":"OUTBOUND-DEST"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(StatusStaged), "test: regression #15 fallback"), "set order staged")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	partial := 37
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID:    "uuid-reg15-fb-partial",
		RemainingUOP: &partial,
	})

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != partial {
		t.Errorf("UOPRemaining = %d, want %d", got.UOPRemaining, partial)
	}
	if got.Manifest == nil {
		t.Fatal("Manifest = nil; want reconstructed single-payload manifest")
	}

	parsed, err := got.ParseManifest()
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(parsed.Items) != 1 {
		t.Fatalf("manifest items = %d, want 1", len(parsed.Items))
	}
	item := parsed.Items[0]
	if item.CatID != bp.Code {
		t.Errorf("manifest item CatID = %q, want %q", item.CatID, bp.Code)
	}
	if item.Quantity != int64(partial) {
		t.Errorf("manifest item Quantity = %d, want %d", item.Quantity, partial)
	}

	// Pre-release manifest had qty=100; post-fix the manifest should NOT
	// still reflect that. Read the raw JSON to be doubly explicit.
	if got.Manifest != nil {
		var raw map[string]any
		if err := json.Unmarshal([]byte(*got.Manifest), &raw); err == nil {
			items, _ := raw["items"].([]any)
			if len(items) > 0 {
				if first, ok := items[0].(map[string]any); ok {
					if q, ok := first["qty"].(float64); ok && int(q) == 100 {
						t.Errorf("manifest still carries pre-release qty=100; reconstruction did not fire")
					}
				}
			}
		}
	}
}

// TestRegression_ProduceIngestUsesRuntimeNotTemplate pins the Phase 0d fix:
// CreateIngestStoreOrder must write the operator-measured runtime UOP value
// (carried in OrderIngestRequest.Quantity from Edge's produce_plan.go) to
// bins.uop_remaining — NOT the payload template's UOPCapacity. The pre-fix
// code at dispatch/lifecycle_service.go:139 used tmpl.UOPCapacity, so a
// finalize that captured 47 cycles on a 100-capacity template would write
// 100 instead of 47. The plant-wide invariant (sum of bins + buckets equals
// physical total) requires the count to reflect what the operator actually
// finalized, not what the bin "should" hold.
func TestRegression_ProduceIngestUsesRuntimeNotTemplate(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	bp.UOPCapacity = 100
	testutil.MustNoErr(t, db.UpdatePayload(bp), "update payload uop capacity")

	bt, _ := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(bp.ID, []int64{bt.ID}), "set payload bin types")

	produceNode := &nodes.Node{Name: "PRODUCE-RUN", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(produceNode), "create produce node")

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-RUN-1", NodeID: &produceNode.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	const operatorMeasured = 47
	d.HandleOrderIngest(testEnvelope(), &protocol.OrderIngestRequest{
		OrderUUID:   "uuid-ingest-runtime",
		PayloadCode: bp.Code,
		BinLabel:    "BIN-RUN-1",
		SourceNode:  produceNode.Name,
		Quantity:    operatorMeasured,
		Manifest: []protocol.IngestManifestItem{
			{PartNumber: bp.Code, Quantity: operatorMeasured, Description: bp.Code},
		},
	})

	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if got.UOPRemaining != operatorMeasured {
		t.Errorf("bin.UOPRemaining = %d, want %d (operator-measured runtime, not tmpl.UOPCapacity=%d)",
			got.UOPRemaining, operatorMeasured, bp.UOPCapacity)
	}
	if got.UOPRemaining == bp.UOPCapacity {
		t.Errorf("bin.UOPRemaining picked up tmpl.UOPCapacity=%d; pre-Phase-0d bug regressed",
			bp.UOPCapacity)
	}
}
