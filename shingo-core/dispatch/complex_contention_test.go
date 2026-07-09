//go:build docker

package dispatch

// Concurrency test for the reserve/confirm split: two orders
// reserveComplexPlan the same single-bin source at once. The reservations unique
// index (per bin) admits exactly one — the winner reserves the bin and completes;
// the loser sees ErrReservationConflict, counts the bin missing (incomplete, NOT
// an error), and — the load-bearing rider — its reconcile does NOT touch the
// winner's hold. The loser re-queues with its own partial intact (here nothing,
// since the sole bin went to the winner) and retries next tick.

import (
	"encoding/json"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

func TestReserveComplexPlan_ConcurrentContention(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	// Source node with a single bin — the contested resource.
	srcNode := &nodes.Node{Name: "CONC-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(srcNode), "create source node")
	onlyBin := testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "BIN-CONC-ONLY")

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

	var steps []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(order1.StepsJSON), &steps), "parse steps")

	ready := make(chan struct{})
	var wg sync.WaitGroup
	outcomes := make([]reserveOutcome, 2)
	errs := make([]error, 2)
	wg.Add(2)
	for i, order := range []*orders.Order{order1, order2} {
		i, order := i, order
		go func() {
			defer wg.Done()
			<-ready // released simultaneously to maximise the race window
			plan := BuildComplexPlan(steps, d.snapshotPickupBins(steps), bp.Code, srcNode.Name)
			_, outcomes[i], errs[i] = d.allocator.reserveComplexPlan(order, plan)
		}()
	}
	close(ready)
	wg.Wait()

	// A lost race is "missing" (holding), never an error.
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d reserve errored: %v (a lost race must be incomplete, not an error)", i, err)
		}
	}

	// Exactly one order completed (won the single bin); the other is holding.
	winners := 0
	winnerIdx := -1
	for i, oc := range outcomes {
		if oc == reserveComplete {
			winners++
			winnerIdx = i
		}
	}
	if winners != 1 {
		t.Fatalf("reserveComplete count = %d, want exactly 1 (single bin, two orders): outcomes=%v", winners, outcomes)
	}
	loser := []*orders.Order{order1, order2}[1-winnerIdx]
	winner := []*orders.Order{order1, order2}[winnerIdx]

	// The bin is reserved by exactly the winner.
	wRes, err := db.ListReservationsByOrder(winner.ID)
	testutil.MustNoErr(t, err, "list winner reservations")
	if len(wRes) != 1 || wRes[0].BinID != onlyBin.ID {
		t.Errorf("winner reservations = %+v, want exactly the contested bin %d", wRes, onlyBin.ID)
	}

	// The loser holds nothing — it neither stole the winner's bin nor released it.
	lRes, err := db.ListReservationsByOrder(loser.ID)
	testutil.MustNoErr(t, err, "list loser reservations")
	if len(lRes) != 0 {
		t.Errorf("loser reservations = %+v, want none (lost the race; must not hold or release the winner's bin)", lRes)
	}
}
