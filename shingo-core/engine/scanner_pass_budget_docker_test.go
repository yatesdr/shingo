//go:build docker

package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/dispatch/plan"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// scanner_pass_budget_docker_test.go — what one fulfillment scanner pass costs
// in STATEMENTS, at a plant-shaped config. EVIDENCE, NOT A PIN.
//
// The numbers are logged under a BUDGET: prefix and recorded, with the phase
// they were taken at, in the Stage A BUDGETS.md. Nothing here asserts a count:
// a cost is measured once and written down, and a pin exists only for a
// statement a change ADDS. The unification phases change what a pass does
// (one-step acquire, then a new ladder), so this harness is the before-and-after
// instrument for that and nothing more.
//
// THE FIXTURE is shaped like a small plant rather than a unit case: six storage
// slots (four holding bins of one payload, two empty), seven line nodes, six
// retrieves to six lines and one move of a bin off a line into storage, all
// waiting for the scanner. Three passes are measured. The first finds material
// for some and not the rest, so it is the pass that dispatches; the second
// re-asks and can still dispatch an order that lost a race inside the first; the
// third is the steady state, where what is left is waiting.
//
// RUN ON DEMAND: go test -tags docker -run TestBudget_ScannerPass -v ./engine/

// quiesce waits until the counting store has been silent for a settle window, so
// a measurement starts after the engine's boot work and ends after any async
// tail a pass kicks off (event handlers, edge sends).
func quiesce(t *testing.T, c *store.QueryCounter) {
	t.Helper()
	const settle = 400 * time.Millisecond
	deadline := time.Now().Add(20 * time.Second)
	last, since := c.Count(), time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if n := c.Count(); n != last {
			last, since = n, time.Now()
			continue
		}
		if time.Since(since) >= settle {
			return
		}
	}
	t.Fatalf("the engine never went quiet (count still moving at %d)", c.Count())
}

func TestBudget_ScannerPass(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	sd := testdb.SetupStandardData(t, db)

	// A market: one NGRP with six storage slots of the fixture's STOR type as
	// direct children (the flattened depth-1 shape), bins in the first four. The
	// retrieves name the group, as a plant's do, so the finder chooses the slot.
	grpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "NGRP type")
	market := &nodes.Node{Name: "BUD-MKT", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(market), "create market group")
	var stor []*nodes.Node
	for i := 1; i <= 6; i++ {
		n := &nodes.Node{Name: fmt.Sprintf("BUD-STOR-%d", i), Zone: "A", Enabled: true,
			NodeTypeID: sd.StorageNode.NodeTypeID, ParentID: &market.ID}
		testutil.MustNoErr(t, db.CreateNode(n), "create storage slot")
		stor = append(stor, n)
	}
	for i := 0; i < 4; i++ {
		testdb.CreateBinAtNode(t, db, sd.Payload.Code, stor[i].ID, fmt.Sprintf("BUD-BIN-%d", i+1))
	}
	var lines []*nodes.Node
	for i := 1; i <= 7; i++ {
		n := &nodes.Node{Name: fmt.Sprintf("BUD-LINE-%d", i), Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), "create line node")
		lines = append(lines, n)
	}
	moveBin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, lines[6].ID, "BUD-BIN-MOVE")

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })

	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, cdb, backend)
	quiesce(t, counter)

	counter.Reset()
	time.Sleep(time.Second)
	t.Logf("BUDGET: background noise over 1s of an idle started engine: %d statements", counter.Count())

	// The orders go in AFTER Start, through the plain handle and with no event,
	// so Start's own boot scan cannot take the dispatching pass, and the next
	// scan the engine would run by itself is the 60s sweep.
	for i := 0; i < 6; i++ {
		testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = fmt.Sprintf("bud-retrieve-%d", i+1)
			o.OrderType = dispatch.OrderTypeRetrieve
			o.Status = protocol.StatusQueued
			o.PayloadCode = sd.Payload.Code
			o.SourceNode = market.Name
			o.DeliveryNode = lines[i].Name
		})
	}
	testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "bud-move"
		o.OrderType = dispatch.OrderTypeMove
		o.Status = protocol.StatusQueued
		o.PayloadCode = sd.Payload.Code
		o.SourceNode = lines[6].Name
		o.DeliveryNode = stor[4].Name
		o.BinID = &moveBin.ID
	})

	measure := func(name string) {
		t.Helper()
		before := len(backend.CreateRequests())
		counter.Reset()
		eng.RunFulfillmentScan()
		sync := counter.Statements()
		quiesce(t, counter)
		all := counter.Statements()
		t.Logf("BUDGET: %s — %d dispatched · synchronous %s", name,
			len(backend.CreateRequests())-before, testutil.StatementBreakdown(sync))
		t.Logf("BUDGET: %s — with async tail: %d statements", name, len(all))
	}

	measure("scanner pass 1 (dispatching)")
	measure("scanner pass 2 (re-ask; can still dispatch what pass 1 raced for)")
	measure("scanner pass 3 (steady state, remaining orders waiting)")

	rows, err := db.DB.Query(`SELECT edge_uuid, status, COALESCE(queue_cause,'') FROM orders ORDER BY id`)
	testutil.MustNoErr(t, err, "read outcomes")
	defer rows.Close()
	for rows.Next() {
		var uuid, status, cause string
		testutil.MustNoErr(t, rows.Scan(&uuid, &status, &cause), "scan outcome")
		t.Logf("BUDGET: outcome %s %s %s", uuid, status, cause)
	}
}

// ── C2c (Lane C): TWO MORE SCENARIOS FOR THIS HARNESS (DESIGN-C2c §8) ──────
//
// EVIDENCE, NOT PINS, written before Lane C's source (D13 "cost check before
// source") and counted at the base. The flat fixture above exercises neither
// the lane gate nor asIntent's group re-choice, which are what C2c changes.
//
// (i)  C2b item 7's shape (REPORT-wedge-rulings-2026-10-08 §3): ten queued
//      complex orders, each with one group drop into a two-lane marked group,
//      parked on a named pickup that has no bin. The C2b harness that produced
//      "101 steady / 10 per queued order per pass" was not found in the tree, so
//      it is rebuilt here. The budget it serves: asIntent at or under +3 reads per
//      queued order per pass (brief, Lane C costs).
// (ii) N waiters at one group's waiting spot, each refused at its own lane, then
//      ONE pickout from another lane of the group through the engine's real
//      subscriber. The budget it serves: the group's slot state read once per
//      group per gate event (DESIGN-C2c §8: ActiveGateCandidates scans N+2 today,
//      1 under C2c).
//
// RUN ON DEMAND: go test -tags docker -run TestBudget_C2c -v ./engine/

// c2cBudgetGroup is a group marked by its spot (testdb.MarkGroup: <group>-WAIT)
// with `lanes` lanes of `depth` slots each, [0] the mouth.
func c2cBudgetGroup(t *testing.T, db *store.DB, prefix string, lanes, depth int) (*nodes.Node, []*nodes.Node, [][]*nodes.Node) {
	t.Helper()
	ngrp, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "NGRP type")
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	testutil.MustNoErr(t, err, "LANE type")
	grp := &nodes.Node{Name: prefix + "-GRP", IsSynthetic: true, Enabled: true, NodeTypeID: &ngrp.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create the group")
	testdb.MarkGroup(t, db, grp.ID)
	var ls []*nodes.Node
	var slots [][]*nodes.Node
	for l := 0; l < lanes; l++ {
		lane := &nodes.Node{Name: fmt.Sprintf("%s-L%d", prefix, l), IsSynthetic: true, Enabled: true,
			NodeTypeID: &laneType.ID, ParentID: &grp.ID}
		testutil.MustNoErr(t, db.CreateNode(lane), "create a lane")
		var ss []*nodes.Node
		for i := 0; i < depth; i++ {
			d := i
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", lane.Name, i), Enabled: true, ParentID: &lane.ID, Depth: &d}
			testutil.MustNoErr(t, db.CreateNode(s), "create a slot")
			ss = append(ss, s)
		}
		ls = append(ls, lane)
		slots = append(slots, ss)
	}
	return grp, ls, slots
}

// c2cBudgetOutcomes logs every order whose edge uuid starts with prefix.
func c2cBudgetOutcomes(t *testing.T, db *store.DB, prefix string) {
	t.Helper()
	rows, err := db.DB.Query(`SELECT edge_uuid, status, COALESCE(queue_cause,''), wait_index, steps_json
		FROM orders WHERE edge_uuid LIKE $1 ORDER BY id`, prefix+"%")
	testutil.MustNoErr(t, err, "read outcomes")
	defer rows.Close()
	for rows.Next() {
		var uuid, status, cause, steps string
		var waitIndex int
		testutil.MustNoErr(t, rows.Scan(&uuid, &status, &cause, &waitIndex, &steps), "scan outcome")
		t.Logf("BUDGET: outcome %s %s %q wait_index=%d steps=%s", uuid, status, cause, waitIndex, steps)
	}
}

func TestBudget_C2cQueuedGroupDrops(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	sd := testdb.SetupStandardData(t, db)
	grp, _, _ := c2cBudgetGroup(t, db, "BUD7", 2, 3)
	const queued = 10
	var named []*nodes.Node
	for i := 1; i <= queued; i++ {
		n := &nodes.Node{Name: fmt.Sprintf("BUD7-PRESS-%d", i), Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), "create a named pickup with no bin")
		named = append(named, n)
	}

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, cdb, backend)
	quiesce(t, counter)

	// After Start, with no event, as above: the boot scan cannot take pass 1.
	for i, n := range named {
		steps, mErr := json.Marshal([]plan.Step{
			{Action: protocol.ActionPickup, Node: n.Name},
			{Action: protocol.ActionDropoff, Node: grp.Name},
		})
		testutil.MustNoErr(t, mErr, "marshal a plan")
		testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = fmt.Sprintf("bud7-%d", i+1)
			o.OrderType = dispatch.OrderTypeComplex
			o.Status = protocol.StatusQueued
			o.PayloadCode = sd.Payload.Code
			o.SourceNode = n.Name
			o.DeliveryNode = grp.Name
			o.StepsJSON = string(steps)
		})
	}

	measure := func(name string) {
		t.Helper()
		before := len(backend.CreateRequests())
		counter.Reset()
		eng.RunFulfillmentScan()
		sync := counter.Statements()
		quiesce(t, counter)
		all := counter.Statements()
		t.Logf("BUDGET: C2c (i) %s — N=%d queued · %d dispatched · synchronous %d (%.1f per queued order) %s",
			name, queued, len(backend.CreateRequests())-before, len(sync), float64(len(sync))/queued,
			testutil.StatementBreakdown(sync))
		t.Logf("BUDGET: C2c (i) %s — with async tail: %d statements", name, len(all))
	}
	measure("pass 1")
	measure("pass 2")
	measure("pass 3 (steady)")
	c2cBudgetOutcomes(t, db, "bud7-")
}

func TestBudget_C2cGroupGateEvent(t *testing.T) {
	const waiters = 4
	db, cfg := testdb.OpenWithConfig(t)
	sd := testdb.SetupStandardData(t, db)
	grp, lanes, slots := c2cBudgetGroup(t, db, "BUDG", waiters+1, 2)
	spot := grp.Name + "-WAIT"
	full := slots[waiters]
	leaving := testdb.CreateBinAtNode(t, db, sd.Payload.Code, full[0].ID, "BUDG-LEAVES")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, full[1].ID, "BUDG-STAYS")
	away := &nodes.Node{Name: "BUDG-AWAY", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(away), "create the node the bin leaves to")

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, cdb, backend)
	quiesce(t, counter)

	// N waiters at G's one spot. Waiter i's drop is pencilled (paper, D13 F1) at
	// the back of lane i, and lane i holds another order's robot (occupancy), so
	// each is refused lane-occupied where it is pencilled. The last lane is full.
	for i := 0; i < waiters; i++ {
		occupant := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = fmt.Sprintf("budg-occupant-%d", i)
			o.DeliveryNode = slots[i][0].Name
			o.Status = "in_transit"
		})
		ok, oErr := reservations.AcquireOccupancy(db.DB, occupant.ID, lanes[i].ID)
		if oErr != nil || !ok {
			t.Fatalf("fixture: occupy lane %s: ok=%v err=%v", lanes[i].Name, ok, oErr)
		}
		line := &nodes.Node{Name: fmt.Sprintf("BUDG-LINE-%d", i), Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(line), "create a line node")
		steps, mErr := json.Marshal([]plan.Step{
			{Action: protocol.ActionWait, Node: line.Name, WaitKind: plan.WaitKindStation},
			{Action: protocol.ActionPickup, Node: line.Name},
			{Action: protocol.ActionWait, Node: spot, WaitKind: plan.WaitKindLane, WaitLane: lanes[i].ID},
			{Action: protocol.ActionDropoff, Node: slots[i][1].Name, Group: grp.Name},
		})
		testutil.MustNoErr(t, mErr, "marshal a waiter's plan")
		w := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = fmt.Sprintf("budg-waiter-%d", i)
			o.StationID = "line-1"
			o.OrderType = dispatch.OrderTypeComplex
			o.PayloadCode = sd.Payload.Code
			o.SourceNode = line.Name
			o.DeliveryNode = slots[i][1].Name
			o.Status = protocol.StatusStaged
		})
		testutil.MustNoErr(t, db.UpdateOrderStepsJSON(w.ID, string(steps)), "persist a waiter's plan")
		testutil.MustNoErr(t, db.UpdateOrderVendor(w.ID, fmt.Sprintf("BUDG-V%d", i), "WAITING", "AMR-1"), "vendor")
		testutil.MustNoErr(t, db.UpdateOrderWaitIndex(w.ID, 1), "stand it at the spot")
		testutil.MustNoErr(t, reservations.AcquireSlot(db.DB, w.ID, slots[i][1].ID, "groupRoom"), "paper")
	}
	// Settle every waiter's refusal before the measured event, uncounted.
	for i := 0; i < waiters; i++ {
		eng.Dispatcher().EvaluateLaneReleases(lanes[i].ID)
	}
	quiesce(t, counter)
	c2cBudgetOutcomes(t, db, "budg-waiter-")

	// THE EVENT: the full lane's mouth bin leaves, announced by the pickout the
	// engine's lane-gate subscriber hears.
	testutil.MustNoErr(t, db.MoveBinClearingStaging(leaving.ID, away.ID, false), "the mouth bin leaves")
	quiesce(t, counter)
	before := len(backend.ReleaseCalls())
	counter.Reset()
	eng.Events.Emit(Event{Type: EventBinEnteredTransit, Payload: BinEnteredTransitEvent{
		BinID: leaving.ID, FromNodeID: full[0].ID,
	}})
	sync := counter.Statements()
	quiesce(t, counter)
	all := counter.Statements()
	t.Logf("BUDGET: C2c (ii) one pickout, N=%d waiters at one group's spot — %d released · synchronous %d %s",
		waiters, len(backend.ReleaseCalls())-before, len(sync), testutil.StatementBreakdown(sync))
	t.Logf("BUDGET: C2c (ii) with async tail: %d statements", len(all))
	c2cBudgetOutcomes(t, db, "budg-waiter-")
}

// ── RULINGS-C2c-run-through §3.3: the gate event's reads, like for like ──────
//
// TestBudget_C2cGroupGateEvent above measured two different events (at the
// base its pickout released nobody, after C2c it released one), so its two
// numbers were not a comparison. These are: one fixture, two events, each the
// same event at the base and after.
//
//	releases-nobody  N waiters at G's spot, each pencilled in its own lane and
//	                 each lane holding another robot; the last lane full and
//	                 holding a robot too. Its mouth bin leaves (the pickout):
//	                 every lane a waiter could take is occupied, so nobody goes.
//	releases-one     the same, and the event is lane 0's robot finishing (its
//	                 order completes): waiter 0 goes into lane 0 — at the base by
//	                 the lane's re-bind and its append, after C2c by the group's
//	                 choice and the release hand — and the others stay.
//
// AND THE PIN THE RULING ASKS FOR: the group's slot state is read ONCE per
// event, however many waiters ask (the per-pass cache serves every waiter after
// the first at 0 reads). A statement that reads the group's per-lane store slots
// is counted, either spelling: the per-owner read it replaced (one per
// waiter, deleted by the rework) and the one read
// (nodes.ReadGroupSlotState). RED at 56a702c6: four per event.

// c2cGateEventFixture builds the like-for-like fixture and returns the counting
// engine's store, its counter and backend, the group's lanes and slots, the
// occupants by lane, and the full lane's mouth bin.
type c2cGateEvent struct {
	db, cdb   *store.DB
	counter   *store.QueryCounter
	backend   *testdb.MockTrackingBackend
	eng       *Engine
	lanes     []*nodes.Node
	slots     [][]*nodes.Node
	occupants []*orders.Order
	mouthBin  int64
	away      *nodes.Node
}

func c2cGateEventFixture(t *testing.T, prefix string, waiters int) c2cGateEvent {
	t.Helper()
	db, cfg := testdb.OpenWithConfig(t)
	sd := testdb.SetupStandardData(t, db)
	grp, lanes, slots := c2cBudgetGroup(t, db, prefix, waiters+1, 2)
	spot := grp.Name + "-WAIT"
	full := slots[waiters]
	away := &nodes.Node{Name: prefix + "-AWAY", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(away), "create the node robots leave to")
	leaving := testdb.CreateBinAtNode(t, db, sd.Payload.Code, full[0].ID, prefix+"-LEAVES")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, full[1].ID, prefix+"-STAYS")

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, cdb, backend)
	quiesce(t, counter)

	var occupants []*orders.Order
	for i := 0; i <= waiters; i++ {
		// A robot storing into the lane's mouth (the full lane's is leaving with
		// nothing): its order finishing is the lane's event, as a placement is.
		into := away.Name
		if i < waiters {
			into = slots[i][0].Name
		}
		occupant := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = fmt.Sprintf("%s-occupant-%d", prefix, i)
			o.DeliveryNode = into
			o.Status = "in_transit"
		})
		ok, oErr := reservations.AcquireOccupancy(db.DB, occupant.ID, lanes[i].ID)
		if oErr != nil || !ok {
			t.Fatalf("fixture: occupy lane %s: ok=%v err=%v", lanes[i].Name, ok, oErr)
		}
		occupants = append(occupants, occupant)
		if i == waiters {
			break // the full lane holds a robot and no waiter
		}
		line := &nodes.Node{Name: fmt.Sprintf("%s-LINE-%d", prefix, i), Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(line), "create a line node")
		steps, mErr := json.Marshal([]plan.Step{
			{Action: protocol.ActionWait, Node: line.Name, WaitKind: plan.WaitKindStation},
			{Action: protocol.ActionPickup, Node: line.Name},
			{Action: protocol.ActionWait, Node: spot, WaitKind: plan.WaitKindLane, WaitLane: lanes[i].ID},
			{Action: protocol.ActionDropoff, Node: slots[i][1].Name, Group: grp.Name},
		})
		testutil.MustNoErr(t, mErr, "marshal a waiter's plan")
		w := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = fmt.Sprintf("%s-waiter-%d", prefix, i)
			o.StationID = "line-1"
			o.OrderType = dispatch.OrderTypeComplex
			o.Coordinated = true
			o.PayloadCode = sd.Payload.Code
			o.SourceNode = line.Name
			o.DeliveryNode = slots[i][1].Name
			o.Status = protocol.StatusStaged
		})
		testutil.MustNoErr(t, db.UpdateOrderStepsJSON(w.ID, string(steps)), "persist a waiter's plan")
		testutil.MustNoErr(t, db.UpdateOrderVendor(w.ID, fmt.Sprintf("%s-V%d", prefix, i), "WAITING", "AMR-1"), "vendor")
		testutil.MustNoErr(t, db.UpdateOrderWaitIndex(w.ID, 1), "stand it at the spot")
		testutil.MustNoErr(t, reservations.AcquireSlot(db.DB, w.ID, slots[i][1].ID, "groupRoom"), "paper")
	}
	// Settle every waiter's refusal before the measured event, uncounted.
	for i := 0; i < waiters; i++ {
		eng.Dispatcher().EvaluateLaneReleases(lanes[i].ID)
	}
	quiesce(t, counter)
	return c2cGateEvent{db: db, cdb: cdb, counter: counter, backend: backend, eng: eng, lanes: lanes,
		slots: slots, occupants: occupants, mouthBin: leaving.ID, away: away}
}

// c2cGroupSlotReads counts the statements that read a group's per-lane store
// slots, in either spelling (see the section comment).
func c2cGroupSlotReads(stmts []string) int {
	n := 0
	for _, s := range stmts {
		if strings.Contains(s, "group-slot-state") || strings.Contains(s, "FROM nodes lane WHERE lane.parent_id") {
			n++
		}
	}
	return n
}

// c2cMeasureEvent runs emit on the counting engine and logs what it cost.
func c2cMeasureEvent(t *testing.T, f c2cGateEvent, what string, waiters int, emit func()) (released, stmts int, read []string) {
	t.Helper()
	before := len(f.backend.ReleaseCalls())
	f.counter.Reset()
	emit()
	sync := f.counter.Statements()
	quiesce(t, f.counter)
	all := f.counter.Statements()
	released = len(f.backend.ReleaseCalls()) - before
	t.Logf("BUDGET: C2c gate event %s, N=%d waiters — %d released · synchronous %d %s",
		what, waiters, released, len(sync), testutil.StatementBreakdown(sync))
	t.Logf("BUDGET: C2c gate event %s with async tail: %d statements; group slot-state reads %d",
		what, len(all), c2cGroupSlotReads(all))
	return released, len(sync), all
}

func TestBudget_C2cGateEventLikeForLike(t *testing.T) {
	const waiters = 4
	t.Run("releases-nobody", func(t *testing.T) {
		f := c2cGateEventFixture(t, "LFLN", waiters)
		testutil.MustNoErr(t, f.db.MoveBinClearingStaging(f.mouthBin, f.away.ID, false), "the mouth bin leaves")
		quiesce(t, f.counter)
		released, _, all := c2cMeasureEvent(t, f, "releases-nobody", waiters, func() {
			f.eng.Events.Emit(Event{Type: EventBinEnteredTransit, Payload: BinEnteredTransitEvent{
				BinID: f.mouthBin, FromNodeID: f.slots[waiters][0].ID,
			}})
		})
		c2cBudgetOutcomes(t, f.db, "LFLN-waiter-")
		if released != 0 {
			t.Fatalf("fixture: the event released %d, want nobody (every lane a waiter could take holds a robot)", released)
		}
		if n := c2cGroupSlotReads(all); n != 1 {
			t.Errorf("the group's slot state was read %d times in one event with %d waiters, want once: the "+
				"per-pass cache serves every waiter after the first at 0 reads (RULINGS-C2c-run-through §3.3)",
				n, waiters)
		}
	})
	t.Run("releases-one", func(t *testing.T) {
		f := c2cGateEventFixture(t, "LFLO", waiters)
		first := f.occupants[0]
		_, err := f.db.TerminalizeOrder(first.ID, protocol.StatusConfirmed, "lane 0's robot finished")
		testutil.MustNoErr(t, err, "lane 0's robot finishes")
		quiesce(t, f.counter)
		released, _, all := c2cMeasureEvent(t, f, "releases-one", waiters, func() {
			f.eng.Events.Emit(Event{Type: EventOrderCompleted, Payload: OrderCompletedEvent{
				OrderID: first.ID, EdgeUUID: first.EdgeUUID,
			}})
		})
		c2cBudgetOutcomes(t, f.db, "LFLO-waiter-")
		if released != 1 {
			t.Fatalf("fixture: the event released %d, want exactly waiter 0 into lane 0", released)
		}
		if n := c2cGroupSlotReads(all); n != 1 {
			t.Errorf("the group's slot state was read %d times in one event with %d waiters, want once "+
				"(RULINGS-C2c-run-through §3.3)", n, waiters)
		}
	})
}
