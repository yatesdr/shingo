//go:build docker

package dispatch

// Complex-order concurrency test: two ApplyComplexPlan calls targeting the same
// pickup node race for the single available bin.
//
// Design note: ApplyComplexPlan has no test hook between ListBinsByNode and
// ClaimForDispatch (unlike PlanningService.postFindHook on the simple path).
// We therefore rely on timing: both goroutines are released simultaneously via
// close(ready), maximising the probability that they interleave ListBinsByNode
// before either commits a claim.
//
// The SQL CAS (WHERE claimed_by IS NULL) guarantees exactly one winner
// regardless of interleaving. The losing goroutine will receive either:
//   - codeClaimFailed  if it saw the bin as unclaimed on read (true race);
//   - codeNoBin        if it saw the bin as claimed by the winner on read
//                      (sequential, BinUnavailableReason pre-filtered it).
//
// Either outcome is a correct implementation; the test pins "exactly one order
// claims the bin" and "the loser's error wraps *planningError".
// See SYNTH-round2.md §ErrRaced for why we don't sentinel-distinguish the two.

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestApplyComplexPlan_ConcurrentPickupContention verifies that two concurrent
// ApplyComplexPlan calls targeting the same pickup node produce exactly one
// successful claim. The loser receives a planningError with Code either
// codeClaimFailed (raced) or codeNoBin (sequential) — both are valid; what
// must never happen is both orders claiming the same bin.
func TestApplyComplexPlan_ConcurrentPickupContention(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	// Source node with a single bin — the contested resource.
	srcNode := &nodes.Node{Name: "CONC-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(srcNode), "create source node")
	testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "BIN-CONC-ONLY")

	mkOrder := func(uuid string) *orders.Order {
		o := &orders.Order{
			EdgeUUID:     uuid,
			StationID:    "line-1",
			OrderType:    OrderTypeComplex,
			Status:       StatusQueued,
			Quantity:     1,
			PayloadCode:  bp.Code,
			SourceNode:   srcNode.Name,
			DeliveryNode: lineNode.Name,
			ProcessNode:  srcNode.Name,
		}
		steps := []resolvedStep{
			{Action: protocol.ActionPickup, Node: srcNode.Name},
			{Action: protocol.ActionDropoff, Node: lineNode.Name},
		}
		stepsJSON, _ := json.Marshal(steps)
		o.StepsJSON = string(stepsJSON)
		testutil.MustNoErr(t, db.CreateOrder(o), "create order "+uuid)
		return o
	}
	order1 := mkOrder("conc-order-1")
	order2 := mkOrder("conc-order-2")

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Parse steps once; reuse across both goroutines (read-only after creation).
	var steps []resolvedStep
	testutil.MustNoErr(t, func() error {
		return json.Unmarshal([]byte(order1.StepsJSON), &steps)
	}(), "parse steps")

	ready := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)

	for i, order := range []*orders.Order{order1, order2} {
		i, order := i, order
		go func() {
			defer wg.Done()
			<-ready // released simultaneously to maximise race window
			pickupBins := d.snapshotPickupBins(steps)
			plan := BuildComplexPlan(steps, pickupBins, bp.Code, srcNode.Name)
			errs[i] = d.ApplyComplexPlan(order, plan, bp.Code, nil)
		}()
	}
	close(ready)
	wg.Wait()

	// Exactly one must succeed.
	winnerIdx := -1
	for i, err := range errs {
		if err == nil {
			if winnerIdx != -1 {
				t.Errorf("multiple winners: goroutine %d and %d both succeeded", winnerIdx, i)
			}
			winnerIdx = i
		}
	}
	if winnerIdx == -1 {
		t.Fatalf("no winner: both goroutines failed (%v)", errs)
	}
	loserIdx := 1 - winnerIdx

	// The bin must be claimed by exactly one order.
	claimed, err := db.ListBinsByClaim(order1.ID)
	testutil.MustNoErr(t, err, "list claimed bins for order1")
	claimed2, err := db.ListBinsByClaim(order2.ID)
	testutil.MustNoErr(t, err, "list claimed bins for order2")

	if len(claimed) > 0 && len(claimed2) > 0 {
		t.Error("both orders have a claimed bin — CAS invariant violated")
	}

	// Loser's error must be a planningError with a recognised claim-failure code.
	var pe *planningError
	if !errors.As(errs[loserIdx], &pe) {
		t.Fatalf("loser error %v does not wrap *planningError", errs[loserIdx])
	}
	// In the concurrent case (true CAS race) pe.Code == codeClaimFailed.
	// In the sequential case (BinUnavailableReason pre-filtered) pe.Code == codeNoBin.
	// Both are valid; what must never happen is any other code (e.g. reshuffle_error).
	if pe.Code != codeClaimFailed && pe.Code != codeNoBin {
		t.Errorf("loser planningError.Code = %q, want %q or %q",
			pe.Code, codeClaimFailed, codeNoBin)
	}
}
