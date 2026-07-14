//go:build docker

package dispatch

import (
	"encoding/json"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
)

// TestSwapRemovalLegHeld_UntilSupplyClaims pins the two-robot swap dispatch
// hold: the removal (evac) leg must not claim the line bin while its supply
// sibling holds no replacement bin, so the line keeps its current bin when
// the supermarket is empty (ALN_003 swap-starvation, 2026-06-03). Once the
// supply leg claims a bin, the hold clears.
func TestSwapRemovalLegHeld_UntilSupplyClaims(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	superNode := &nodes.Node{Name: "SWAP-SUPER", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(superNode), "create super node")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Supply leg: delivers TO the line (DeliveryNode == ProcessNode). Left
	// queued — no source bin staged in the supermarket.
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "swap-supply", PayloadCode: bp.Code, Quantity: 1, ProcessNode: lineNode.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: superNode.Name},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})
	supply, err := db.GetOrderByUUID("swap-supply")
	testutil.MustNoErr(t, err, "get supply leg")

	// The line bin the removal leg would pull — present, so only the hold
	// stops the claim.
	lineBin := &bins.Bin{BinTypeID: 1, Label: "SWAP-LINE-BIN", NodeID: &lineNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(lineBin), "create line bin")
	testutil.MustNoErr(t, db.SetBinManifest(lineBin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, bp.Code, 40), "set manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(lineBin.ID, ""), "confirm manifest")

	// Removal leg: pickup@line -> dropoff@super (DeliveryNode != ProcessNode),
	// carrying the supply sibling's UUID so Core pairs them at intake.
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "swap-removal", PayloadCode: bp.Code, Quantity: 1, ProcessNode: lineNode.Name,
		SiblingOrderUUID: "swap-supply",
		Steps: []protocol.ComplexOrderStep{
			{Action: "wait", Node: lineNode.Name},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: superNode.Name},
		},
	})
	removal, err := db.GetOrderByUUID("swap-removal")
	testutil.MustNoErr(t, err, "get removal leg")

	// Gate fires: this removal leg takes the line's bin and has only one pickup
	// (it cannot fetch its own replacement), and the supply has no claimed bin.
	removalSteps, ok := decodeSteps(removal.StepsJSON)
	if !ok {
		t.Fatal("removal leg has no readable steps — intake contract changed")
	}
	if held, _ := d.swapRemovalLegHeld(removal, removalSteps); !held {
		t.Fatal("removal leg should be held while supply sibling has no claimed bin")
	}
	// DispatchPreparedComplex must stay queued without claiming the line bin.
	if derr := d.DispatchPreparedComplex(removal); derr == nil {
		t.Fatal("DispatchPreparedComplex should return the hold error")
	}
	removal, _ = db.GetOrderByUUID("swap-removal")
	if removal.Status != StatusQueued {
		t.Errorf("removal status = %q, want queued", removal.Status)
	}
	if removal.BinID != nil {
		t.Errorf("removal claimed bin %d while held — the line bin must stay put", *removal.BinID)
	}

	// Supply secures a replacement bin (claimed_by supply) → the hold clears.
	superBin := &bins.Bin{BinTypeID: 1, Label: "SWAP-SUPER-BIN", NodeID: &superNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(superBin), "create super bin")
	if _, err := db.DB.Exec(`UPDATE bins SET claimed_by=$1 WHERE id=$2`, supply.ID, superBin.ID); err != nil {
		t.Fatalf("claim super bin for supply: %v", err)
	}
	removal, _ = db.GetOrderByUUID("swap-removal")
	if held, _ := d.swapRemovalLegHeld(removal, removalSteps); held {
		t.Error("removal leg should no longer be held once supply has a claimed bin")
	}
}

// TestFullDepletion_ClearsManifest verifies that when a move order carries
// remaining_uop=0 (fully depleted bin), the manifest is atomically cleared
// and the bin is claimed in a single operation.
func TestFullDepletion_ClearsManifest(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	// Create a line node (process node) and a filled bin there
	processNode := &nodes.Node{Name: "LINE1-CONSUME", Enabled: true}
	db.CreateNode(processNode)

	bin := &bins.Bin{BinTypeID: 1, Label: "BIN-DEP-1", NodeID: &processNode.ID, Status: "staged"}
	db.CreateBin(bin)
	db.SetBinManifest(bin.ID, `{"items":[{"catid":"PART-A","qty":100}]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")

	// Verify bin starts with manifest
	before, _ := db.GetBin(bin.ID)
	if before.PayloadCode == "" {
		t.Fatal("bin should have payload_code before depletion")
	}

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	// Build an envelope with remaining_uop=0 (fully depleted)
	zero := 0
	orderReq := &protocol.OrderRequest{
		OrderUUID:    "uuid-dep-1",
		OrderType:    "move",
		Quantity:     1,
		SourceNode:   processNode.Name,
		DeliveryNode: lineNode.Name,
		PayloadCode:  bp.Code,
		RemainingUOP: &zero,
	}
	body, _ := json.Marshal(orderReq)
	env := testEnvelope()
	env.Payload, _ = json.Marshal(protocol.Data{Body: body})

	d.HandleOrderRequest(env, orderReq)
	// The claim-move to the scanner: the scanner (mirrored) is the single claim
	// point and seeds the manifest clear from the persisted remaining_uop=0.
	dispatchSimpleViaScanner(t, d, db, "uuid-dep-1")

	// Verify bin's manifest is cleared AND bin is claimed
	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty (cleared after full depletion)", got.PayloadCode)
	}
	if got.UOPRemaining != 0 {
		t.Errorf("UOPRemaining = %d, want 0", got.UOPRemaining)
	}
	if got.ManifestConfirmed {
		t.Error("ManifestConfirmed should be false after clear")
	}
	if got.ClaimedBy == nil {
		t.Error("bin should be claimed after dispatch")
	}
}

// TestPartialConsumption_SyncsUOP verifies that when a move order carries
// remaining_uop>0 (partially consumed bin), the UOP is synced to the bin
// record while preserving the manifest and payload_code.
func TestPartialConsumption_SyncsUOP(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	processNode := &nodes.Node{Name: "LINE1-PARTIAL", Enabled: true}
	db.CreateNode(processNode)

	bin := &bins.Bin{BinTypeID: 1, Label: "BIN-PRT-1", NodeID: &processNode.ID, Status: "staged"}
	db.CreateBin(bin)
	manifest := `{"items":[{"catid":"PART-A","qty":100}]}`
	db.SetBinManifest(bin.ID, manifest, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	// remaining_uop=42 — partial consumption
	partial := 42
	orderReq := &protocol.OrderRequest{
		OrderUUID:    "uuid-prt-1",
		OrderType:    "move",
		Quantity:     1,
		SourceNode:   processNode.Name,
		DeliveryNode: lineNode.Name,
		PayloadCode:  bp.Code,
		RemainingUOP: &partial,
	}
	body, _ := json.Marshal(orderReq)
	env := testEnvelope()
	env.Payload, _ = json.Marshal(protocol.Data{Body: body})

	d.HandleOrderRequest(env, orderReq)
	// The claim-move to the scanner: intake persists remaining_uop and queues; the
	// scanner (mirrored here) is the single claim point and seeds the manifest sync
	// from it. This is the end-to-end proof the operator's release-correction count
	// survives the claim-move — the differential tests pin fleet route, not the
	// manifest.
	dispatchSimpleViaScanner(t, d, db, "uuid-prt-1")

	got, _ := db.GetBin(bin.ID)

	// UOP should be synced
	if got.UOPRemaining != 42 {
		t.Errorf("UOPRemaining = %d, want 42", got.UOPRemaining)
	}

	// Manifest should be preserved
	if got.PayloadCode != bp.Code {
		t.Errorf("PayloadCode = %q, want %q (preserved)", got.PayloadCode, bp.Code)
	}
	if got.Manifest == nil {
		t.Error("Manifest should be preserved after partial consumption")
	} else {
		// Postgres JSONB normalizes whitespace/key order, so compare decoded values
		var gotJSON, wantJSON any
		json.Unmarshal([]byte(*got.Manifest), &gotJSON)
		json.Unmarshal([]byte(manifest), &wantJSON)
		gotBytes, _ := json.Marshal(gotJSON)
		wantBytes, _ := json.Marshal(wantJSON)
		if string(gotBytes) != string(wantBytes) {
			t.Errorf("Manifest = %s, want %s", *got.Manifest, manifest)
		}
	}

	// Should be claimed
	if got.ClaimedBy == nil {
		t.Error("bin should be claimed after dispatch")
	}
}

// TestConcurrentRetrieveEmpty_BothClaimed_NoOverlap verifies that when two
// retrieve_empty orders race for two available bins, each order claims a
// different bin with no double-claims. This tests concurrent claim distribution
// rather than the ghost-bin TOCTOU (which ClearAndClaim's atomic SQL prevents).
//
// The Find→Claim pair is intentionally non-atomic, so two goroutines can Find
// the SAME bin before either Claims. Production tolerates that via
// claimFirstAvailable, which retries the remaining candidates when a claim
// loses the claimed_by guard; this test models the same retry so the outcome
// is deterministic (the loser re-Finds — the winner's bin is now claimed and
// skipped — and takes the other empty). Without the retry the test was flaky
// under -race, asserting 2 claims but occasionally getting 1 on a collision.
func TestConcurrentRetrieveEmpty_BothClaimed_NoOverlap(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, _ = setupTestData(t, db)

	// Create a payload with bin type compatibility
	bp := &payloads.Payload{Code: "RACE-BP"}
	db.CreatePayload(bp)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	storageNode := &nodes.Node{Name: "RACE-STORAGE", Zone: "A", Enabled: true}
	db.CreateNode(storageNode)

	// Create two empty bins
	bin1 := &bins.Bin{BinTypeID: bt.ID, Label: "RACE-BIN-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin1)
	bin2 := &bins.Bin{BinTypeID: bt.ID, Label: "RACE-BIN-2", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin2)

	// Create two orders that will race to claim
	order1 := &orders.Order{EdgeUUID: "race-1", StationID: "test", OrderType: protocol.OrderTypeRetrieveEmpty, Status: "pending", Quantity: 1, DeliveryNode: "LINE1-IN"}
	db.CreateOrder(order1)
	order2 := &orders.Order{EdgeUUID: "race-2", StationID: "test", OrderType: protocol.OrderTypeRetrieveEmpty, Status: "pending", Quantity: 1, DeliveryNode: "LINE1-IN"}
	db.CreateOrder(order2)

	// Race: two goroutines try to find and claim empty bins concurrently
	var wg sync.WaitGroup
	results := make([]int64, 2)
	errors := make([]error, 2)

	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(idx int) {
			defer wg.Done()
			orderID := order1.ID
			if idx == 1 {
				orderID = order2.ID
			}
			// Retry on a lost claim race, mirroring production's
			// claimFirstAvailable. Bounded so a genuine "no empties left"
			// condition terminates rather than spinning; with two bins and
			// two goroutines each loses at most once before claiming the other.
			for attempt := 0; attempt < 4; attempt++ {
				found, err := db.FindEmptyCompatibleBin(bp.Code, "", 0)
				if err != nil || found == nil {
					if err != nil {
						errors[idx] = err
					}
					return
				}
				// Acquire the pending reservation the demoted-CAS ClaimBin
				// guard now requires. On a lost race another order already
				// holds the active (unique-per-bin) reservation, so re-Find —
				// the reserved bin is now excluded — and take the next.
				if rerr := reservations.Acquire(db, orderID, found.ID, "test"); rerr != nil {
					continue
				}
				if cerr := db.ClaimBin(found.ID, orderID); cerr != nil {
					errors[idx] = cerr
					return
				}
				if cerr := reservations.Confirm(db, orderID, found.ID); cerr != nil {
					errors[idx] = cerr
					return
				}
				results[idx] = found.ID
				return
			}
		}(i)
	}
	wg.Wait()

	// Count successful claims
	successCount := 0
	for i := 0; i < 2; i++ {
		if errors[i] == nil && results[i] > 0 {
			successCount++
		}
	}

	// Both should succeed (two empty bins available)
	if successCount != 2 {
		t.Errorf("expected 2 successful claims (2 empty bins), got %d", successCount)
	}

	// Verify each bin is claimed by exactly one order
	got1, _ := db.GetBin(bin1.ID)
	got2, _ := db.GetBin(bin2.ID)
	if got1.ClaimedBy == nil && got2.ClaimedBy == nil {
		t.Error("expected at least one bin to be claimed")
	}
	// They should have different claimants
	if got1.ClaimedBy != nil && got2.ClaimedBy != nil && *got1.ClaimedBy == *got2.ClaimedBy {
		t.Error("both bins claimed by same order — race condition")
	}
}

// TestComplexOrder_RemainingUOP_ProcessNodeOnly verifies that in a complex
// order with multiple pickups, only the pickup at the process node gets
// remainingUOP applied. Other pickups (storage, staging) get plain claims.
//
// SKIPPED: Phase 4b of bin-transit-state moved bin claiming off the
// synchronous HandleComplexOrderRequest path into the scanner-driven
// DispatchPreparedComplex path. The protocol.ComplexOrderRequest's
// RemainingUOP is consumed by HandleOrderRelease at operator-release
// time, not at intake — Edge's CreateComplexOrder doesn't thread it
// through at submit. Re-enable when RemainingUOP-at-intake gets
// persisted on the order row (the queued-then-replayed path needs to
// recover it from somewhere across the queue boundary).
func TestComplexOrder_RemainingUOP_ProcessNodeOnly(t *testing.T) {
	t.Parallel()
	t.Skip("Phase 4b: RemainingUOP-at-intake deferred until persisted on order row")
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	processNode := &nodes.Node{Name: "COMPLEX-LINE1", Enabled: true}
	db.CreateNode(processNode)
	stagingNode := &nodes.Node{Name: "COMPLEX-STAGING", Enabled: true}
	db.CreateNode(stagingNode)

	// Bin at process node (outgoing, depleted bin)
	binProcess := &bins.Bin{BinTypeID: 1, Label: "BIN-CP-1", NodeID: &processNode.ID, Status: "staged"}
	db.CreateBin(binProcess)
	db.SetBinManifest(binProcess.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(binProcess.ID, "")

	// Bin at staging node (incoming, should NOT have manifest changed)
	binStaging := &bins.Bin{BinTypeID: 1, Label: "BIN-CP-2", NodeID: &stagingNode.ID, Status: "staged"}
	db.CreateBin(binStaging)
	db.SetBinManifest(binStaging.ID, `{"items":[{"catid":"NEW","qty":200}]}`, bp.Code, 200)
	db.ConfirmBinManifest(binStaging.ID, "")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	zero := 0
	env := testEnvelope()
	submitComplexAndDispatch(t, d, db, env, &protocol.ComplexOrderRequest{
		OrderUUID:   "uuid-complex-pn",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: processNode.Name},   // outgoing depleted bin
			{Action: "dropoff", Node: "COMPLEX-STAGING"}, // park old nearby
			{Action: "pickup", Node: stagingNode.Name},   // grab new from staging
			{Action: "dropoff", Node: processNode.Name},  // deliver new to line
		},
		RemainingUOP: &zero,
	})

	// Process node bin: should have manifest CLEARED (remainingUOP=0 applied)
	gotProcess, _ := db.GetBin(binProcess.ID)
	if gotProcess.PayloadCode != "" {
		t.Errorf("process bin PayloadCode = %q, want empty (cleared)", gotProcess.PayloadCode)
	}
	if gotProcess.ClaimedBy == nil {
		t.Error("process bin should be claimed")
	}

	// Staging node bin: should have manifest PRESERVED (plain claim, no remainingUOP)
	gotStaging, _ := db.GetBin(binStaging.ID)
	if gotStaging.PayloadCode != bp.Code {
		t.Errorf("staging bin PayloadCode = %q, want %q (preserved)", gotStaging.PayloadCode, bp.Code)
	}
	if gotStaging.UOPRemaining != 200 {
		t.Errorf("staging bin UOPRemaining = %d, want 200 (preserved)", gotStaging.UOPRemaining)
	}
	if gotStaging.ClaimedBy == nil {
		t.Error("staging bin should be claimed")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// HandleOrderRelease + RemainingUOP integration tests.
//
// These tests stage a complex order with a wait step (so it lands in
// StatusStaged with the line bin claimed by ApplyComplexPlan), then call
// HandleOrderRelease with various RemainingUOP values to assert the
// late-binding manifest sync runs correctly before the fleet release.
//
// Maps to BinManifestService.SyncOrClearForReleased's three branches plus
// the "wrong owner" failure surface (operator clicks release on an order
// whose bin has been reassigned to someone else).
// ──────────────────────────────────────────────────────────────────────────

// stageComplexOrderWithLineBin sets up a complex order whose first non-wait
// pickup is at the line node, dispatches it through HandleComplexOrderRequest
// (which claims the line bin), then forces the order into StatusStaged so
// HandleOrderRelease will accept it.
func stageComplexOrderWithLineBin(t *testing.T, db *store.DB, d *Dispatcher, lineNode *nodes.Node, bp *payloads.Payload, orderUUID, binLabel string) (*orders.Order, *bins.Bin) {
	t.Helper()

	// Destination node for the dropoff step (must exist for step resolution).
	destNode := &nodes.Node{Name: "RELEASE-DEST", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(destNode), "create dest node")

	// Filled bin at the line (outgoing partial/empty after consumption).
	bin := &bins.Bin{BinTypeID: 1, Label: binLabel, NodeID: &lineNode.ID, Status: "staged"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin %s: %v", binLabel, err)
	}
	if err := db.SetBinManifest(bin.ID, `{"items":[{"catid":"PART-A","qty":100}]}`, bp.Code, 100); err != nil {
		t.Fatalf("set manifest %s: %v", binLabel, err)
	}
	if err := db.ConfirmBinManifest(bin.ID, ""); err != nil {
		t.Fatalf("confirm manifest %s: %v", binLabel, err)
	}

	env := testEnvelope()
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   orderUUID,
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "wait", Node: lineNode.Name},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: destNode.Name},
		},
		// nil at creation — release path is what we're testing.
	})

	// Phase 4b of bin-transit-state: HandleComplexOrderRequest now
	// creates the order in `queued` and lets the fulfillment scanner
	// drive the bin claim via DispatchPreparedComplex. The
	// dispatcher-only test harness has no scanner (newTestDispatcher
	// doesn't wire an engine bus), so we call DispatchPreparedComplex
	// directly to mirror what the scanner would do in production.
	order, err := db.GetOrderByUUID(orderUUID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status == StatusQueued {
		testutil.MustNoErr(t, d.DispatchPreparedComplex(order), "dispatch prepared complex")
		order, err = db.GetOrderByUUID(orderUUID)
		if err != nil {
			t.Fatalf("re-get order after dispatch: %v", err)
		}
	}
	if order.BinID == nil {
		t.Fatalf("expected order.BinID to be set by ApplyComplexPlan; got nil")
	}
	if *order.BinID != bin.ID {
		t.Fatalf("expected order to claim bin %d, got %d", bin.ID, *order.BinID)
	}

	// Force StatusStaged so HandleOrderRelease accepts the release.
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(StatusStaged), "test: simulate robot waiting"), "set order staged")
	order, _ = db.GetOrderByUUID(orderUUID)
	return order, bin
}

func TestHandleOrderRelease_RemainingUOPZeroClearsManifest(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	_, bin := stageComplexOrderWithLineBin(t, db, d, lineNode, bp, "uuid-rel-zero", "BIN-REL-ZERO")

	// NOTHING PULLED disposition → remaining_uop=0 → manifest cleared.
	zero := 0
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID:    "uuid-rel-zero",
		RemainingUOP: &zero,
	})

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty (cleared on release)", got.PayloadCode)
	}
	if got.UOPRemaining != 0 {
		t.Errorf("UOPRemaining = %d, want 0", got.UOPRemaining)
	}
	if got.ManifestConfirmed {
		t.Error("ManifestConfirmed should be false after release-clear")
	}
	// Claim must remain — release does not unclaim.
	if got.ClaimedBy == nil {
		t.Error("ClaimedBy should be preserved after release-clear")
	}
}

func TestHandleOrderRelease_RemainingUOPPositiveSyncsUOP(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	_, bin := stageComplexOrderWithLineBin(t, db, d, lineNode, bp, "uuid-rel-pos", "BIN-REL-POS")

	// SEND PARTIAL BACK disposition → remaining_uop=positive → UOP synced,
	// manifest preserved.
	partial := 800
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID:    "uuid-rel-pos",
		RemainingUOP: &partial,
	})

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 800 {
		t.Errorf("UOPRemaining = %d, want 800 (synced from release)", got.UOPRemaining)
	}
	if got.PayloadCode != bp.Code {
		t.Errorf("PayloadCode = %q, want %q (preserved)", got.PayloadCode, bp.Code)
	}
	if got.Manifest == nil {
		t.Error("Manifest should be preserved on partial-back release")
	}
	// NOTE: this assertion does not check manifest CONTENT — it would pass
	// even if the manifest still carried the pre-release qty. The strengthened
	// content assertion lives in TestRegression_15_PartialBackReconstructsManifest
	// (uop_regression_test.go), currently t.Skip'd until fix #15 lands.
}

func TestHandleOrderRelease_RemainingUOPNilLeavesManifestAlone(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	_, bin := stageComplexOrderWithLineBin(t, db, d, lineNode, bp, "uuid-rel-nil", "BIN-REL-NIL")

	before, _ := db.GetBin(bin.ID)

	// Legacy / Order-A path: nil remaining_uop → no manifest action.
	// Preserves pre-Phase-8 behavior: release dispatches without touching
	// the bin's manifest.
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID: "uuid-rel-nil",
		// RemainingUOP omitted (nil)
	})

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != before.PayloadCode {
		t.Errorf("PayloadCode = %q, want %q (untouched on nil release)", got.PayloadCode, before.PayloadCode)
	}
	if got.UOPRemaining != before.UOPRemaining {
		t.Errorf("UOPRemaining = %d, want %d (untouched on nil release)", got.UOPRemaining, before.UOPRemaining)
	}
}

// TestHandleOrderRelease_BinIDNilFallbackClearsManifest verifies the
// source-node fallback path. Setup: an order with order.BinID=nil but a
// bin sitting at order.SourceNode (the line). This is the production
// failure mode for two-robot Order B observed on ALN_002 plant test
// 2026-04-23 — ApplyComplexPlan didn't populate BinID, but the bin is
// physically at the line and the operator's release wants its manifest
// cleared. Without the fallback, HandleOrderRelease silently skipped the
// sync and the bin landed at OutboundDestination still tagged.
//
// With the fallback: HandleOrderRelease detects BinID==nil, looks up the
// bin at order.SourceNode, calls SyncOrClearForReleasedNoOwner (which
// drops the claim guard since the fallback bin isn't claimed by this
// order), and the manifest clears.
func TestHandleOrderRelease_BinIDNilFallbackClearsManifest(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	// Bin physically at the line with manifest intact (the OLD bin
	// the line consumed down to zero — Edge knows it's empty, Core
	// still has the loaded state because there's no cycle telemetry).
	bin := &bins.Bin{BinTypeID: 1, Label: "BIN-FALLBACK", NodeID: &lineNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")
	testutil.MustNoErr(t, db.SetBinManifest(bin.ID, `{"items":[{"catid":"PART-A","qty":100}]}`, bp.Code, 100), "set manifest")

	// Order whose BinID is nil but whose SourceNode points at the line.
	// Mimics the production failure mode where ApplyComplexPlan didn't
	// claim a bin for the order at creation time.
	order := &orders.Order{
		EdgeUUID:     "uuid-fallback-clear",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusStaged,
		Quantity:     1,
		SourceNode:   lineNode.Name,
		DeliveryNode: "OUTBOUND-DEST",
		PayloadCode:  bp.Code,
		StepsJSON:    `[{"action":"wait","node":"` + lineNode.Name + `"},{"action":"pickup","node":"` + lineNode.Name + `"},{"action":"dropoff","node":"OUTBOUND-DEST"}]`,
		// BinID intentionally nil
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	// Force StatusStaged (CreateOrder may default to pending).
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(StatusStaged), "test: fallback scenario"), "set order staged")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Operator release with NOTHING PULLED → remaining_uop=0.
	zero := 0
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID:    "uuid-fallback-clear",
		RemainingUOP: &zero,
	})

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty (cleared via source-node fallback)", got.PayloadCode)
	}
	if got.UOPRemaining != 0 {
		t.Errorf("UOPRemaining = %d, want 0 (cleared via fallback)", got.UOPRemaining)
	}
	// The bin was NOT claimed by this order — the no-owner variant
	// should leave claimed_by alone (it was nil, stays nil).
	if got.ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v, want nil (fallback should not claim)", got.ClaimedBy)
	}
}

// TestDispatchPreparedComplex_QueuesOnFullConcreteStorageDropoff pins the #1
// fix (regression 2b05dce): a complex changeover drop/evac whose final
// dropoff is a FULL concrete storage slot must queue, not dispatch into the
// occupied slot. The scanner dropped the capacity gate for every complex
// order to unstick two-robot supply legs; the gate is restored here, scoped
// to concrete storage/staging dropoffs.
func TestDispatchPreparedComplex_QueuesOnFullConcreteStorageDropoff(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	// Concrete storage slot under an NGRP, occupied → CheckDropoffCapacity blocks.
	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("NGRP node type: %v", err)
	}
	grp := &nodes.Node{Name: "DROP-NGRP", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create NGRP")
	slot := &nodes.Node{Name: "DROP-NGRP-S1", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(slot), "create storage slot")
	occupant := &bins.Bin{BinTypeID: 1, Label: "DROP-OCCUPANT", NodeID: &slot.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(occupant), "occupy slot")

	order := &orders.Order{
		EdgeUUID:     "uuid-drop-full",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     1,
		SourceNode:   lineNode.Name,
		DeliveryNode: slot.Name,
		PayloadCode:  bp.Code,
		StepsJSON: `[{"action":"wait","node":"` + lineNode.Name + `"},` +
			`{"action":"pickup","node":"` + lineNode.Name + `"},` +
			`{"action":"dropoff","node":"` + slot.Name + `"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(StatusQueued), "test: queued"), "set queued")
	order, _ = db.GetOrder(order.ID)

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	if err := d.DispatchPreparedComplex(order); err == nil {
		t.Fatal("expected a queue (non-nil error) on a full concrete storage dropoff, got nil (dispatched)")
	}

	got, _ := db.GetOrder(order.ID)
	if got.Status != StatusQueued {
		t.Errorf("status = %q, want queued — a full storage dropoff must not dispatch", got.Status)
	}
	if got.QueueReason == "" {
		t.Errorf("queue_reason empty, want a dropoff-capacity reason")
	}
	if got.VendorOrderID != "" {
		t.Errorf("VendorOrderID = %q, want empty (not dispatched)", got.VendorOrderID)
	}
}

// TestIsConcreteStorageDropoff_RoleGate pins the role classifier that scopes
// the #1 capacity gate. The two-robot guard is the line-node case: a LINE /
// production dropoff must classify false so a supply leg is NOT capacity-
// gated (a sibling evac clears the line; Core can't model that), which is the
// deadlock 2b05dce removed. NGRP roots classify false too — they're handled
// by step re-resolution before the gate.
func TestIsConcreteStorageDropoff_RoleGate(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("NGRP node type: %v", err)
	}
	grp := &nodes.Node{Name: "CLS-NGRP", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create NGRP")
	slot := &nodes.Node{Name: "CLS-NGRP-S1", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")

	cases := []struct {
		name string
		node string
		want bool
	}{
		{"concrete storage slot (child of NGRP)", slot.Name, true},
		{"line/production node — two-robot guard", lineNode.Name, false},
		{"synthetic NGRP root — handled by re-resolution", grp.Name, false},
		{"empty", "", false},
		{"missing node", "NO-SUCH-NODE", false},
	}
	for _, c := range cases {
		if got := isConcreteStorageDropoff(d.db, c.node); got != c.want {
			t.Errorf("%s: isConcreteStorageDropoff(%q) = %v, want %v", c.name, c.node, got, c.want)
		}
	}
}

// TestHandleOrderRelease_BinIDNilFallbackPrefersClaim is the Phase 3
// regression test for the bin-transit-state project. The fallback now
// prefers a claim-based lookup (claimed_by = order.ID) over the node-
// based one — critical because under transit semantics a claimed bin's
// node_id is _TRANSIT, not the original source node, so the old node-
// only fallback would silently miss the bin and the operator's release
// would fail to clear/sync the manifest.
//
// Setup: order has BinID=nil (so we go through the fallback), but a
// bin IS claimed by this order, sitting at a node OTHER than
// sourceNode (mimicking transit). The fallback must find it via claim,
// not via node lookup.
func TestHandleOrderRelease_BinIDNilFallbackPrefersClaim(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	// Set up a "transit" node so the bin can sit somewhere other than
	// the line. Doesn't have to be the synthetic _TRANSIT — any non-
	// source node proves the claim-first lookup works regardless of
	// where the bin physically is.
	transitNode := &nodes.Node{Name: "TRANSIT-TEST", IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(transitNode), "create transit node")

	bin := &bins.Bin{BinTypeID: 1, Label: "BIN-CLAIM-FB", NodeID: &transitNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")
	testutil.MustNoErr(t, db.SetBinManifest(bin.ID, `{"items":[{"catid":"PART-A","qty":50}]}`, bp.Code, 50), "set manifest")

	// Order with BinID=nil but the bin IS claimed by this order. Mimics
	// the DB-write race where ClaimForDispatch took but UpdateOrderBinID
	// failed, OR the transit-state scenario where a complex order's
	// bin has moved to _TRANSIT mid-flight before the operator's release.
	order := &orders.Order{
		EdgeUUID:     "uuid-claim-fb",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusStaged,
		Quantity:     1,
		SourceNode:   lineNode.Name,
		ProcessNode:  lineNode.Name,
		DeliveryNode: "OUTBOUND-DEST",
		PayloadCode:  bp.Code,
		StepsJSON:    `[{"action":"wait","node":"` + lineNode.Name + `"},{"action":"pickup","node":"` + lineNode.Name + `"},{"action":"dropoff","node":"OUTBOUND-DEST"}]`,
		// BinID intentionally nil — fallback path will fire.
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(StatusStaged), "test: claim-first fallback"), "set order staged")
	// Set the claim AFTER the order exists so claimed_by points at a real ID.
	if _, err := db.Exec(`UPDATE bins SET claimed_by=$1 WHERE id=$2`, order.ID, bin.ID); err != nil {
		t.Fatalf("set claimed_by: %v", err)
	}

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	zero := 0
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID:    "uuid-claim-fb",
		RemainingUOP: &zero,
	})

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty — claim-first fallback should have cleared the manifest of the in-transit claimed bin (node-only lookup at sourceNode would have missed it because the bin is at TRANSIT-TEST, not the line)", got.PayloadCode)
	}
	if got.UOPRemaining != 0 {
		t.Errorf("UOPRemaining = %d, want 0 (cleared via claim-first fallback)", got.UOPRemaining)
	}
}

// TestHandleOrderRelease_BinIDNilFallbackSyncsPartial verifies the
// fallback path for SEND PARTIAL BACK (positive remaining_uop).
func TestHandleOrderRelease_BinIDNilFallbackSyncsPartial(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	bin := &bins.Bin{BinTypeID: 1, Label: "BIN-FALLBACK-PART", NodeID: &lineNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")
	manifest := `{"items":[{"catid":"PART-A","qty":100}]}`
	testutil.MustNoErr(t, db.SetBinManifest(bin.ID, manifest, bp.Code, 100), "set manifest")

	order := &orders.Order{
		EdgeUUID:     "uuid-fallback-partial",
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
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(StatusStaged), "test: fallback partial scenario"), "set order staged")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	partial := 37
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID:    "uuid-fallback-partial",
		RemainingUOP: &partial,
	})

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 37 {
		t.Errorf("UOPRemaining = %d, want 37 (synced via fallback)", got.UOPRemaining)
	}
	if got.PayloadCode != bp.Code {
		t.Errorf("PayloadCode = %q, want %q (preserved on partial-back)", got.PayloadCode, bp.Code)
	}
	// NOTE: see TestRegression_15_PartialBackFallbackReconstructsManifest
	// (uop_regression_test.go) for the strengthened content assertion that
	// fails on current code and passes once #15 lands.
}

// TestHandleOrderRelease_InTransitWithNoMoreSegmentsIsNoOp guards the
// regression observed at Springfield 2026-04-30 (ALN_002 toast: "order must
// be staged to release, got in_transit"). Edge's two-robot consolidated
// release (ReleaseStagedOrders, post-2026-04-27) fans out to both legs of
// a swap unconditionally; Order A is routinely already in_transit by the
// time the operator clicks. Core must accept the duplicate and return a
// no-op success rather than rejecting with an "invalid_state" error that
// surfaces in the HMI and forces the operator to fail one of the orders.
//
// Setup: a single-wait complex order forced into StatusInTransit with
// WaitIndex=1 (i.e. the only wait was already consumed during a prior
// release). Release fires; we expect no error reply enqueued and no
// fleet-side block append (splitSegment returns nil for WaitIndex past
// the final wait).
func TestHandleOrderRelease_InTransitWithNoMoreSegmentsIsNoOp(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	order, _ := stageComplexOrderWithLineBin(t, db, d, lineNode, bp, "uuid-in-transit-noop", "BIN-IN-TRANSIT-NOOP")

	// Simulate the prior release having already happened: order.Status is
	// in_transit and WaitIndex has advanced past the only wait.
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(StatusInTransit), "test: prior release consumed the wait"), "force in_transit")
	testutil.MustNoErr(t, db.UpdateOrderWaitIndex(order.ID, 1), "advance wait_index")

	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID: "uuid-in-transit-noop",
		// RemainingUOP nil — Order A path in the consolidated fan-out.
	})

	msgs, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	for _, m := range msgs {
		if m.MsgType == string(protocol.TypeOrderError) {
			t.Errorf("unexpected error reply enqueued: %s", string(m.Payload))
		}
	}

	// WaitIndex must not advance further — there's nothing to dispatch.
	got, _ := db.GetOrder(order.ID)
	if got.WaitIndex != 1 {
		t.Errorf("WaitIndex = %d, want 1 (no-op should not advance past final wait)", got.WaitIndex)
	}
	if got.Status != StatusInTransit {
		t.Errorf("Status = %q, want in_transit (no-op should not change status)", got.Status)
	}
}

// TestHandleOrderRelease_FaultedLegIsNoOp pins the A1 fix for the ALN_003
// release divergence (Springfield 2026-06-12). Edge's two-robot consolidated
// release fans out to both legs; when the delivery leg has bumper-faulted
// (SEER FAILED → Core's non-terminal `faulted`), Core must NOT reply with an
// `invalid_state` error: Edge converts any non-`manifest_sync_failed` order-error
// into a terminal StatusFailed, killing the Edge mirror while Core's own order
// stays alive and recovers — the exact divergence observed. Symmetric to the
// in_transit no-op above: no error reply enqueued, status untouched, nothing
// dispatched.
func TestHandleOrderRelease_FaultedLegIsNoOp(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	order, _ := stageComplexOrderWithLineBin(t, db, d, lineNode, bp, "uuid-faulted-noop", "BIN-FAULTED-NOOP")

	// Simulate the delivery robot bumper-faulting mid-drop: SEER FAILED → Core
	// `faulted` (recoverable), as rds/poller maps it.
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(StatusFaulted), "test: robot bumper-faulted mid-drop"), "force faulted")
	before, _ := db.GetOrder(order.ID)

	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID: "uuid-faulted-noop",
	})

	msgs, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	for _, m := range msgs {
		if m.MsgType == string(protocol.TypeOrderError) {
			t.Errorf("faulted leg must not enqueue an order-error (Edge would terminally fail the mirror): %s", string(m.Payload))
		}
	}

	// Pure no-op: status stays faulted, wait index untouched.
	got, _ := db.GetOrder(order.ID)
	if got.Status != StatusFaulted {
		t.Errorf("Status = %q, want faulted (no-op must not change status)", got.Status)
	}
	if got.WaitIndex != before.WaitIndex {
		t.Errorf("WaitIndex changed %d -> %d; faulted release must be a pure no-op", before.WaitIndex, got.WaitIndex)
	}
}

// TestHandleOrderRelease_InTransitMultiWaitDispatchesNextSegment verifies
// that the relaxed precondition still does the right thing for a true
// multi-wait order: when an in_transit order has more waits to consume,
// the next segment is dispatched and WaitIndex advances. This exercises
// the design intent documented at HandleOrderRelease.
func TestHandleOrderRelease_InTransitMultiWaitDispatchesNextSegment(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	destNode := &nodes.Node{Name: "MULTI-WAIT-DEST", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(destNode), "create dest node")

	// Two-wait choreography: wait → pickup → wait → dropoff. WaitIndex=1
	// means the first wait was already consumed; the next release should
	// dispatch the segment between wait[1] and end.
	order := &orders.Order{
		EdgeUUID:     "uuid-multi-wait",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusInTransit,
		Quantity:     1,
		SourceNode:   lineNode.Name,
		DeliveryNode: destNode.Name,
		PayloadCode:  bp.Code,
		StepsJSON: `[{"action":"wait","node":"` + lineNode.Name + `"},` +
			`{"action":"pickup","node":"` + lineNode.Name + `"},` +
			`{"action":"wait"},` +
			`{"action":"dropoff","node":"` + destNode.Name + `"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	testutil.MustNoErr(t, db.UpdateOrderVendor(order.ID, "vendor-multi-wait", "DISPATCHED", ""), "set vendor")
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(StatusInTransit), "test: mid-choreography"), "set in_transit")
	testutil.MustNoErr(t, db.UpdateOrderWaitIndex(order.ID, 1), "set wait_index")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID: "uuid-multi-wait",
	})

	msgs, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	for _, m := range msgs {
		if m.MsgType == string(protocol.TypeOrderError) {
			t.Errorf("unexpected error reply enqueued: %s", string(m.Payload))
		}
	}

	got, _ := db.GetOrder(order.ID)
	if got.WaitIndex != 2 {
		t.Errorf("WaitIndex = %d, want 2 (multi-wait re-release should advance)", got.WaitIndex)
	}
}

// TestDispatchPreparedComplex_NoSourceBinSkipsNotFails pins the new
// architecture: a complex order whose every pickup node is empty (the
// "bin was removed externally before dispatch" condition) lands in
// terminal StatusSkipped via SkipOrderAtomic, NOT StatusFailed via
// FailOrderAtomic. The semantic difference matters operationally —
// Skipped feeds the changeover-task auto-advance path on Edge instead
// of surfacing a sticky red error to the operator.
func TestDispatchPreparedComplex_NoSourceBinSkipsNotFails(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	// Source and destination nodes exist; source has NO bin (the bin was
	// pulled to quality hold before this dispatch tick).
	sourceNode := &nodes.Node{Name: "ALN-EMPTY-1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(sourceNode), "create source node")
	destNode := &nodes.Node{Name: "SMN-OUT-1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(destNode), "create dest node")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Stage a complex evac order at the empty source. Shape matches the
	// changeover_planner.go BuildStagedReleaseSteps output: wait → pickup
	// → dropoff. lineNode here stands in for the unused but required
	// "process node" tracking context.
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID:   "uuid-evac-empty",
		PayloadCode: bp.Code,
		Quantity:    1,
		ProcessNode: sourceNode.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: "wait", Node: sourceNode.Name},
			{Action: "pickup", Node: sourceNode.Name},
			{Action: "dropoff", Node: destNode.Name},
		},
	})
	_ = lineNode

	order, err := db.GetOrderByUUID("uuid-evac-empty")
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != StatusQueued {
		t.Fatalf("pre-dispatch status = %q, want %q", order.Status, StatusQueued)
	}

	// Now drive the dispatcher. Source is empty → no_source_bin → Skip.
	_ = d.DispatchPreparedComplex(order)

	got, err := db.GetOrderByUUID("uuid-evac-empty")
	if err != nil {
		t.Fatalf("re-get order: %v", err)
	}
	if got.Status != StatusSkipped {
		t.Errorf("status = %q, want %q (no_source_bin must route to Skip, not Fail)", got.Status, StatusSkipped)
	}
	if got.ErrorDetail == "" {
		t.Error("ErrorDetail should be populated with the planning-error detail")
	}
}

// TestDispatchPreparedComplex_BinClaimedElsewhereFails complements the test
// above: when bins ARE at the source but every one is rejected (claimed
// by another order, payload mismatch, etc.), the order still terminates
// as Failed — that's an alarm, not a no-op. Pins the gate so a future
// refactor can't accidentally collapse the two cases.
func TestDispatchPreparedComplex_BinClaimedElsewhereHolds(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	sourceNode := &nodes.Node{Name: "ALN-CLAIMED-1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(sourceNode), "create source node")
	destNode := &nodes.Node{Name: "SMN-OUT-2", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(destNode), "create dest node")

	// Bin at source, but claimed by a different order (id=999 — doesn't
	// matter that the row doesn't exist; the dispatcher only looks at the
	// claim_by pointer for the unavailability check).
	bin := &bins.Bin{BinTypeID: 1, Label: "BIN-CLAIMED-1", NodeID: &sourceNode.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")
	testutil.MustNoErr(t, db.SetBinManifest(bin.ID, `{"items":[{"catid":"PART-A","qty":50}]}`, bp.Code, 50), "set manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(bin.ID, ""), "confirm manifest")
	bogusOrderID := int64(999999)
	if _, err := db.DB.Exec(`UPDATE bins SET claimed_by=$1 WHERE id=$2`, bogusOrderID, bin.ID); err != nil {
		t.Fatalf("set claimed_by: %v", err)
	}

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID:   "uuid-evac-claimed",
		PayloadCode: bp.Code,
		Quantity:    1,
		ProcessNode: sourceNode.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: "wait", Node: sourceNode.Name},
			{Action: "pickup", Node: sourceNode.Name},
			{Action: "dropoff", Node: destNode.Name},
		},
	})

	order, _ := db.GetOrderByUUID("uuid-evac-claimed")
	_ = d.DispatchPreparedComplex(order)

	// Behavior change: a bin present but claimed by another order
	// is no longer a terminal Fail — the reserve holds the order in `sourcing` and
	// the scanner retries it, because the contested bin frees when the other order
	// completes (operator-driven hold-and-retry). A claimer that is actually dead/gone
	// (like the bogus 999999 here) is cleared by the orphan-claim reaper, which
	// then lets the retry through. Was: StatusFailed.
	got, _ := db.GetOrderByUUID("uuid-evac-claimed")
	if got.Status != StatusSourcing {
		t.Errorf("status = %q, want %q (bin present but claimed elsewhere now HOLDS for retry, not Fail)", got.Status, StatusSourcing)
	}
}
