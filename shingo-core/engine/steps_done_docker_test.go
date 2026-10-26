//go:build docker

package engine

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/seerrds"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/rds"
	"shingocore/store"
	"shingocore/store/orders"
)

// stepsDoneOf reads orders.steps_done as the boards will: off the row.
func stepsDoneOf(t *testing.T, db *store.DB, orderID int64) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT steps_done FROM orders WHERE id = $1`, orderID).Scan(&n), "read steps_done")
	return n
}

// untilSeen waits for the subscriber to have recorded n blocks of the order.
func untilSeen(t *testing.T, db *store.DB, orderID int64, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		seen := 0
		for _, c := range subscriberSaw(t, db, orderID) {
			seen += c
		}
		if seen >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the subscriber never recorded %d blocks of order %d", n, orderID)
}

// TestStepsDone_TheSubscriberCountsThePlanStepsTheFleetFinished drives the
// production path of TestBlockFinished_EveryBlockSentReachesTheSubscriber and
// reads orders.steps_done after each block the vendor finishes: by position,
// the plan's next open step when the block is that step's action at its node.
func TestStepsDone_TheSubscriberCountsThePlanStepsTheFleetFinished(t *testing.T) {
	t.Parallel()

	t.Run("a plain order with no stored plan, its load leg an advanced load sequence", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		storageNode, lineNode, bp := setupTestData(t, db)
		createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-DONE-SEQ")
		mustExec(t, db, `UPDATE payloads SET advanced_load_sequence = 'Child cart interlock' WHERE code = $1`, bp.Code)
		vendor, url := newStubVendor(t)
		eng := newTestEngine(t, db, seerrds.New(seerrds.Config{BaseURL: url, Timeout: 2 * time.Second, PollInterval: 10 * time.Millisecond}))

		eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
			OrderUUID: "done-seq", OrderType: dispatch.OrderTypeRetrieve, PayloadCode: bp.Code,
			DeliveryNode: lineNode.Name, Quantity: 1,
		})
		order := testdb.RequireOrderStatus(t, db, "done-seq", dispatch.StatusDispatched)
		if order.StepsJSON != "" {
			t.Fatalf("fixture: the plain order stored a plan %s; this case is the one with none", order.StepsJSON)
		}
		// Go_AP1, Spin_90, load, Spin_inverse_90 at the source, then the drop.
		for _, step := range []struct{ finished, want int }{{2, 0}, {3, 1}, {4, 1}} {
			vendor.drive(order.VendorOrderID, rds.StateRunning, step.finished)
			untilSeen(t, db, order.ID, step.finished)
			if got := stepsDoneOf(t, db, order.ID); got != step.want {
				t.Fatalf("after %d of the load sequence's blocks steps_done is %d, want %d", step.finished, got, step.want)
			}
		}
		vendor.drive(order.VendorOrderID, rds.StateFinished, 5)
		untilStatus(t, db, "done-seq", dispatch.StatusDelivered)
		if got := stepsDoneOf(t, db, order.ID); got != 2 {
			t.Errorf("delivered, steps_done is %d, want 2: the lift and the drop", got)
		}
	})

	waits := []struct {
		name                string
		wait                protocol.ComplexOrderStep
		staged, lifted, all int // steps_done parked, after the lift past the wait, delivered
	}{
		{"a bare wait: no block, the lift after it counts past it",
			protocol.ComplexOrderStep{Action: protocol.ActionWait}, 2, 4, 5},
		{"a wait at the line: its own block",
			protocol.ComplexOrderStep{Action: protocol.ActionWait, Node: "LINE1-IN"}, 3, 4, 5},
	}
	for i, c := range waits {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			storageNode, lineNode, bp := setupTestData(t, db)
			createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-DONE-WAIT-"+string(rune('A'+i)))
			vendor, url := newStubVendor(t)
			eng := newTestEngine(t, db, seerrds.New(seerrds.Config{BaseURL: url, Timeout: 2 * time.Second, PollInterval: 10 * time.Millisecond}))
			d := eng.Dispatcher()
			env := testEnvelope()
			uuid := "done-wait-" + string(rune('a'+i))
			d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
				OrderUUID: uuid, PayloadCode: bp.Code, Quantity: 1,
				Steps: []protocol.ComplexOrderStep{
					{Action: protocol.ActionPickup, Node: storageNode.Name},
					{Action: protocol.ActionDropoff, Node: lineNode.Name},
					c.wait,
					{Action: protocol.ActionPickup, Node: lineNode.Name},
					{Action: protocol.ActionDropoff, Node: storageNode.Name},
				},
			})
			order := testdb.RequireOrderStatus(t, db, uuid, dispatch.StatusDispatched)
			sent := len(vendor.sent(order.VendorOrderID))
			vendor.drive(order.VendorOrderID, rds.StateRunning, 0)
			untilStatus(t, db, uuid, dispatch.StatusInTransit)
			vendor.drive(order.VendorOrderID, rds.StateWaiting, sent)
			untilStatus(t, db, uuid, dispatch.StatusStaged)
			if got := stepsDoneOf(t, db, order.ID); got != c.staged {
				t.Fatalf("parked at the wait, steps_done is %d, want %d", got, c.staged)
			}

			d.HandleOrderRelease(env, &protocol.OrderRelease{OrderUUID: uuid})
			vendor.drive(order.VendorOrderID, rds.StateRunning, sent+1)
			untilSeen(t, db, order.ID, sent+1)
			if got := stepsDoneOf(t, db, order.ID); got != c.lifted {
				t.Fatalf("released and lifted at the line, steps_done is %d, want %d", got, c.lifted)
			}
			vendor.drive(order.VendorOrderID, rds.StateFinished, sent+2)
			untilStatus(t, db, uuid, dispatch.StatusDelivered)
			if got := stepsDoneOf(t, db, order.ID); got != c.all {
				t.Errorf("delivered, steps_done is %d, want %d", got, c.all)
			}
		})
	}
}

// TestBlockCompleted_OneWriteForTheStepItFinishes counts what steps_done costs
// the block subscriber: the order it reads already, and one UPDATE when the
// block finishes a step, none when it finishes none. Before steps_done the
// subscriber spent 2 statements on a Wait block and 2 on a turn of a load
// sequence (the order, the mission event); after, 3 and 2.
func TestBlockCompleted_OneWriteForTheStepItFinishes(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	o := &orders.Order{EdgeUUID: "done-count", StationID: "line-1", OrderType: "complex", Status: protocol.StatusInTransit, Quantity: 1,
		StepsJSON: `[{"action":"pickup","node":"S"},{"action":"dropoff","node":"L"},{"action":"wait","node":"L"},` +
			`{"action":"pickup","node":"L"},{"action":"dropoff","node":"S"}]`}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")
	mustExec(t, db, `UPDATE orders SET steps_done = 2 WHERE id = $1`, o.ID)
	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	eng := newUnstartedEngine(t, cdb, simulator.New())

	cases := []struct {
		task          string
		writes, total int
		done          int
	}{
		{"Wait", 1, 3, 3},    // the wait at the line: step 3 done
		{"Spin_90", 0, 2, 3}, // a turn finishes no step
	}
	for _, c := range cases {
		counter.Reset()
		eng.handleBlockCompleted(BlockCompletedEvent{OrderID: o.ID, VendorOrderID: "v-count", BlockID: "v-count-b3", Location: "L", BinTask: c.task})
		stmts := counter.Statements()
		writes := 0
		for _, s := range stmts {
			s = strings.TrimSpace(s)
			if !strings.Contains(s, "steps_done") || strings.HasPrefix(s, "SELECT") {
				continue // the order's own read names the column too
			}
			writes++
			if !strings.HasPrefix(s, "UPDATE orders SET steps_done = GREATEST(steps_done, $2)") {
				t.Errorf("%s block: steps_done written by %.120q, want the one GREATEST update", c.task, s)
			}
		}
		t.Logf("%s block: %d statements, %d of them steps_done", c.task, len(stmts), writes)
		if writes != c.writes || len(stmts) != c.total {
			t.Errorf("%s block: %d statements, %d writing steps_done; want %d and %d", c.task, len(stmts), writes, c.total, c.writes)
		}
		if got := stepsDoneOf(t, db, o.ID); got != c.done {
			t.Errorf("%s block: steps_done %d, want %d", c.task, got, c.done)
		}
	}
}

// hold puts an order on the stub vendor as Core would find it after a restart:
// the blocks already sent, the first `finished` of them FINISHED.
func (v *stubVendor) hold(id string, state rds.OrderState, finished int, blocks ...rds.Block) {
	v.mu.Lock()
	o := &rds.OrderDetail{ID: id, Vehicle: "AMR-01", State: rds.StateCreated}
	o.Blocks = appendStubBlocks(nil, blocks)
	v.orders[id] = o
	v.mu.Unlock()
	v.drive(id, state, finished)
}

// replayPlan lifts at SMN_A twice: a block re-reported for the first lift is
// the same action at the same node as the plan's next open step once the
// bare wait between them is passed over, so a replay that counted would
// count the second lift before it happened.
const replayPlan = `[{"action":"pickup","node":"SMN_A"},{"action":"dropoff","node":"SLN_B"},{"action":"wait"},` +
	`{"action":"pickup","node":"SMN_A"},{"action":"dropoff","node":"ALN_C"}]`

// statusRows counts the order's mission events of one vendor state: the row
// every committed transition writes, after the blocks of the same poll.
func statusRows(t *testing.T, db *store.DB, orderID int64, state string) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM mission_events WHERE order_id = $1 AND new_state = $2`,
		orderID, state).Scan(&n), "count status rows")
	return n
}

// TestStepsDone_ARestartThatReplaysFinishedBlocksCountsNothing: a restarted
// Core's poller starts with no block states, so its first poll reports every
// block the vendor already finished as newly FINISHED. Those blocks were
// counted before the restart; counted again, the first lift's replay matches
// the second lift and steps_done jumps past work the robot has not done.
func TestStepsDone_ARestartThatReplaysFinishedBlocksCountsNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	vendor, url := newStubVendor(t)
	o := &orders.Order{EdgeUUID: "replay-1", StationID: "line-1", OrderType: "complex", Status: protocol.StatusInTransit,
		Quantity: 1, SourceNode: "SMN_A", DeliveryNode: "ALN_C", StepsJSON: replayPlan}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")
	testutil.MustNoErr(t, db.UpdateOrderVendor(o.ID, "v-replay-1", "RUNNING", "AMR-01"), "vendor order")
	vendor.hold("v-replay-1", rds.StateRunning, 0,
		rds.Block{BlockID: "v-replay-1-b1", Location: "SMN_A", BinTask: "JackLoad"},
		rds.Block{BlockID: "v-replay-1-b2", Location: "SLN_B", BinTask: "JackUnload"})
	fleetAt := func() *seerrds.Adapter {
		return seerrds.New(seerrds.Config{BaseURL: url, Timeout: 2 * time.Second, PollInterval: 10 * time.Millisecond})
	}

	// Started and stopped by hand, as TestCoreRestart_* does: Stop is the
	// restart, and the cleanup newTestEngine registers would stop it twice.
	first := newUnstartedEngine(t, db, fleetAt())
	first.Start()
	vendor.drive("v-replay-1", rds.StateWaiting, 2)
	untilStatus(t, db, "replay-1", dispatch.StatusStaged)
	if got := stepsDoneOf(t, db, o.ID); got != 2 {
		t.Fatalf("fixture: before the restart steps_done is %d, want 2 (the lift and the drop)", got)
	}
	first.Stop()

	// Core restarts on the same database against the same vendor, which still
	// reports both blocks FINISHED and the order WAITING.
	newTestEngine(t, db, fleetAt())
	deadline := time.Now().Add(10 * time.Second)
	for statusRows(t, db, o.ID, string(rds.StateWaiting)) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the restarted Core never polled the order")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := stepsDoneOf(t, db, o.ID); got != 2 {
		t.Errorf("after the restart's replay steps_done is %d, want 2: a replayed block was counted again", got)
	}
	if saw := subscriberSaw(t, db, o.ID); saw["v-replay-1-b1"] != 1 || saw["v-replay-1-b2"] != 1 {
		t.Errorf("after the restart each block has %v BLOCK_FINISHED rows, want one each", saw)
	}
}

// TestBlockCompleted_AReplayedBlockWritesNothing counts a block's statements
// the first time the subscriber sees it and when it sees it again. The first
// time: the order, the leg row and the count, 3. Again: the order and the leg
// insert, which finds the block's row and writes nothing, 2, and steps_done
// stands. The replay is the case that matters: its block is the same action
// at the same node as the next open step.
func TestBlockCompleted_AReplayedBlockWritesNothing(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	o := &orders.Order{EdgeUUID: "replay-count", StationID: "line-1", OrderType: "complex", Status: protocol.StatusInTransit,
		Quantity: 1, StepsJSON: `[{"action":"wait","node":"AP7"},{"action":"pickup","node":"SMN_A"},{"action":"wait"},` +
			`{"action":"wait","node":"AP7"},{"action":"dropoff","node":"ALN_C"}]`}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")
	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	eng := newUnstartedEngine(t, cdb, simulator.New())
	wait := BlockCompletedEvent{OrderID: o.ID, VendorOrderID: "v-rc", BlockID: "v-rc-b1", Location: "AP7", BinTask: "Wait"}

	count := func(what string) (total, writes int) {
		counter.Reset()
		eng.handleBlockCompleted(wait)
		for _, s := range counter.Statements() {
			s = strings.TrimSpace(s)
			if strings.HasPrefix(s, "UPDATE orders SET steps_done") {
				writes++
			}
		}
		t.Logf("%s: %d statements, %d writing steps_done", what, len(counter.Statements()), writes)
		return len(counter.Statements()), writes
	}
	if total, writes := count("the wait block, first time"); total != 3 || writes != 1 {
		t.Errorf("first time: %d statements, %d steps_done writes; want 3 and 1", total, writes)
	}
	if got := stepsDoneOf(t, db, o.ID); got != 1 {
		t.Fatalf("first time: steps_done %d, want 1", got)
	}
	mustExec(t, db, `UPDATE orders SET steps_done = 2 WHERE id = $1`, o.ID) // the lift since
	if total, writes := count("the wait block, replayed"); total != 2 || writes != 0 {
		t.Errorf("replayed: %d statements, %d steps_done writes; want 2 and 0", total, writes)
	}
	if got := stepsDoneOf(t, db, o.ID); got != 2 {
		t.Errorf("replayed: steps_done %d, want 2: the replay was counted as the second wait", got)
	}
	if saw := subscriberSaw(t, db, o.ID); saw["v-rc-b1"] != 1 {
		t.Errorf("the replay wrote a second leg row for the block: %v", saw)
	}
}
