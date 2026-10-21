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

// TestPinSimOperator_CoreHeldWaitIsNotReleased: a leg the Edge authored,
// staged where Core says it holds the leg, is never pressed by the sim
// operator. Core holds it two ways the Edge is told about in OrderStaged's
// wait_kind: a lane wait, and a vacate hold (Core took the station's release
// and appends the segment once a named bin has left the drop's node,
// dispatch/vacate_hold.go). A kind this Edge does not know is Core's too
// (protocol.IsCoreWaitKind). Core refuses a station release of any of them
// (invalid_state), the Edge rolls the refusal back to staged, and an operator
// that pressed it would press again on every sweep until the hold ended — the
// LC7 loop (TestPinSimOperator_LaneHeldChildIsNotReleased) for a leg that is
// not LaneHeld, since it carries the Edge's own steps.
//
// The station-kind row is the control: the same leg at a station wait IS
// pressed, so a zero on the Core rows is the skip and not a fixture that
// never reaches the press.
func TestPinSimOperator_CoreHeldWaitIsNotReleased(t *testing.T) {
	for _, c := range []struct {
		kind      string
		wantPress bool
	}{
		{protocol.WaitKindStation, true},
		{protocol.WaitKindVacate, false},
		{protocol.WaitKindLane, false},
		{"supervisor", false}, // a kind added on a newer Core
	} {
		t.Run(c.kind, func(t *testing.T) {
			got := simPressesAtWaitKind(t, c.kind)
			switch {
			case c.wantPress && got == 0:
				t.Fatalf("control: the sim operator never pressed a leg at a %q wait — the fixture does not "+
					"reach the release, so the Core-held rows check nothing", c.kind)
			case !c.wantPress && got != 0:
				t.Errorf("the sim operator pressed a leg staged at a %q wait %d times; Core holds it and refuses "+
					"every press (invalid_state), and the rollback re-stages it. Want 0 (protocol.IsCoreWaitKind "+
					"in simOperator.release).", c.kind, got)
			}
		})
	}
}

// simPressesAtWaitKind stages an Edge-authored leg (non-empty steps, so not
// LaneHeld) at a wait of the given kind, as Core's OrderStaged records it,
// drives the sim operator's two entry points for a fixed number of cycles,
// plays Core's refusal back after each, and returns how many releases the
// operator attempted.
func simPressesAtWaitKind(t *testing.T, kind string) int {
	t.Helper()
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
		ProcessID: 1, CoreNodeName: "CH_001", Code: "CH1", Name: "CH_001", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	uuid := "core-held-" + kind
	_, err = storeorders.UpsertProjection(db.DB, storeorders.ProjectionRow{
		UUID: uuid, OrderType: protocol.OrderTypeComplex, Status: string(protocol.StatusStaged),
		ProcessNodeID: &nodeID, Quantity: 1, SourceNode: "SYN_MARKET", DeliveryNode: "CH_001",
		PayloadCode: "ASSY",
	})
	testutil.MustNoErr(t, err, "project leg")
	steps := `[{"action":"pickup","node":"SYN_MARKET"},{"action":"wait","node":"CH_STAGE","wait_kind":"station"},` +
		`{"action":"dropoff","node":"CH_001"}]`
	_, err = db.Exec(`UPDATE orders SET steps_json=? WHERE uuid=?`, steps, uuid)
	testutil.MustNoErr(t, err, "write the Edge's steps")
	var stationWait *int
	if kind == protocol.WaitKindStation {
		zero := 0
		stationWait = &zero
	}
	testutil.MustNoErr(t, eng.orderMgr.RecordWaitPoint(uuid, stationWait, kind), "record Core's wait point")
	o, err := db.GetOrderByUUID(uuid)
	testutil.MustNoErr(t, err, "read leg")
	if o.LaneHeld || o.WaitKind != kind {
		t.Fatalf("fixture: LaneHeld=%v wait_kind=%q, want false and %q — the test would check the wrong guard",
			o.LaneHeld, o.WaitKind, kind)
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
	// coreRefuses plays Core's invalid_state reply back, as the Edge handles it,
	// and puts back the wait point Core would report again.
	coreRefuses := func() {
		cur, err := db.GetOrder(o.ID)
		testutil.MustNoErr(t, err, "read leg")
		if cur.Status == protocol.StatusInTransit {
			testutil.MustNoErr(t, eng.orderMgr.RollbackReleaseRejection(uuid, "invalid_state: held by Core"),
				"roll back the refused release")
		}
		testutil.MustNoErr(t, eng.orderMgr.RecordWaitPoint(uuid, stationWait, kind), "re-record the wait point")
	}
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

	const cycles = 6
	for i := 0; i < cycles; i++ {
		if i%2 == 0 {
			op.mu.Lock()
			op.releasing[o.ID] = true // as scheduleRelease does before it spawns the worker
			op.mu.Unlock()
			op.runRelease(o.ID)
		} else {
			op.reconcile()
			drained()
		}
		coreRefuses()
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("release attempts at a %q wait over %d cycles: %d", kind, cycles, attempts)
	return attempts
}
