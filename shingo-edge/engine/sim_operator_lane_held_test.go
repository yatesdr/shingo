//go:build sim

package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingoedge/config"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// TestPinSimOperator_LaneHeldChildIsNotReleased is LC7.
//
// A Core-created child leg sits `staged` on the Edge with no Edge-authored step
// plan, so it is LaneHeld: Core owns its wait and refuses a release of it
// (dispatch.HandleOrderRelease, reply invalid_state). The sim operator used to
// release it anyway, the Edge rolled the refusal back to `staged`
// (RollbackReleaseRejection), and the next sweep scheduled the release again,
// forever: 36,188 refusals for one child move order on the shingo-dev sim,
// about 79 a minute.
//
// The test drives both entry points, runRelease directly and reconcile's sweep,
// for a fixed number of cycles. Each cycle plays Core's refusal back: a leg the
// release moved to in_transit is rolled back to staged, as the order.error
// reply does. It counts the release attempts the operator made.
//
// At 5c0beb74 (before the LaneHeld check) the count is one per cycle; after,
// it is 0.
func TestPinSimOperator_LaneHeldChildIsNotReleased(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)

	var mu sync.Mutex
	attempts := 0
	count := func(f string, a ...any) {
		if strings.Contains(fmt.Sprintf(f, a...), "operator auto-release order") {
			mu.Lock()
			attempts++
			mu.Unlock()
		}
	}
	eng.logFn = count
	eng.debugFn = count

	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: 1, CoreNodeName: "LH_001", Code: "LH1", Name: "LH_001", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	const uuid = "lc7-child-leg"
	_, err = storeorders.UpsertProjection(db.DB, storeorders.ProjectionRow{
		UUID: uuid, OrderType: protocol.OrderType("move"), Status: string(protocol.StatusStaged),
		ProcessNodeID: &nodeID, Quantity: 1, SourceNode: "SYN_MARKET", DeliveryNode: "LH_001",
		PayloadCode: "ASSY",
	})
	testutil.MustNoErr(t, err, "project child leg")
	o, err := db.GetOrderByUUID(uuid)
	testutil.MustNoErr(t, err, "read child leg")
	if !o.LaneHeld {
		t.Fatalf("fixture: the projected staged leg is not LaneHeld (status=%s) — the test checks nothing", o.Status)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	op := &simOperator{
		e:          eng,
		ops:        config.SimOperatorsConfig{SwapRelease: time.Millisecond},
		clk:        clock.Real(),
		ctx:        ctx,
		pending:    make(map[int64]bool),
		releasing:  make(map[int64]bool),
		confirming: make(map[int64]bool),
	}

	// coreRefuses plays Core's invalid_state reply back, as the Edge handles it.
	rollbacks := 0
	coreRefuses := func() {
		cur, err := db.GetOrder(o.ID)
		testutil.MustNoErr(t, err, "read leg")
		if cur.Status == protocol.StatusInTransit {
			testutil.MustNoErr(t, eng.orderMgr.RollbackReleaseRejection(uuid, "invalid_state: order is a leg of a compound order"),
				"roll back the refused release")
			rollbacks++
		}
	}
	// drained waits for reconcile's release worker to finish.
	drained := func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			op.mu.Lock()
			busy := op.releasing[o.ID]
			op.mu.Unlock()
			if !busy {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("reconcile's release worker did not finish")
	}

	const cycles = 10
	for i := 0; i < cycles; i++ {
		if i%2 == 0 {
			op.releasing[o.ID] = true // as scheduleRelease does before it spawns the worker
			op.runRelease(o.ID)
		} else {
			op.reconcile()
			drained()
		}
		coreRefuses()
	}

	mu.Lock()
	got := attempts
	mu.Unlock()
	t.Logf("release attempts on a LaneHeld child leg over %d cycles: %d (refusals rolled back: %d)", cycles, got, rollbacks)
	if got != 0 {
		t.Errorf("the sim operator released a LaneHeld leg %d times in %d cycles; Core refuses every one "+
			"(invalid_state) and the rollback re-stages it, so this repeats forever. Want 0.", got, cycles)
	}
}
