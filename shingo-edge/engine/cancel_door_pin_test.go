package engine

// cancel_door_pin_test.go — one pin a door that cancels an order (final brief
// 5g), each on rows whose status is Core's versioned word: the rows a cancel
// that asks Core can reach (a row with no versioned word keeps the cancel it
// has). Each pin records four things: the tile's orders in the station JSON
// after the tap; the order.cancel the Edge queued, as the JSON object it sends;
// Core's end, played through the wire handler as Core tells a station's cancel
// (order.update cancelled at the next version, then order.cancelled with the
// status Core held); and what the door's own work did while Core had not
// answered.

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/messaging"
	"shingoedge/orders"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// doorPin is an engine whose order manager announces on the engine's bus and
// whose handlers are wired, as engine.New's are, so a door's cancel runs the
// completion chain, and Core's word reaches it through the wire handler.
type doorPin struct {
	t   *testing.T
	eng *Engine
	db  *store.DB
	h   *messaging.EdgeHandler
	// toasts is every Core-cancel toast (OrderCancelledByCoreEvent), by order.
	toasts []string
}

func newDoorPin(t *testing.T, db *store.DB, eng *Engine) *doorPin {
	t.Helper()
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()
	return adoptDoorPin(t, db, eng)
}

// adoptDoorPin is newDoorPin for a fixture that already wired the engine's
// handlers and then gave it the announcing order manager (withCascade): the
// manager's hooks are set as wireEventHandlers sets them.
func adoptDoorPin(t *testing.T, db *store.DB, eng *Engine) *doorPin {
	t.Helper()
	eng.orderMgr.OnStagedMessage = eng.onStagedMessage
	eng.orderMgr.OnDeliveredMessage = eng.onDeliveredMessage
	eng.orderMgr.OnCancelledMessage = eng.onCancelledMessage
	p := &doorPin{t: t, eng: eng, db: db, h: newEdgeHandlerFor(eng)}
	eng.Events.Subscribe(func(evt Event) {
		if c, ok := evt.Payload.(OrderCancelledByCoreEvent); ok {
			p.toasts = append(p.toasts, fmt.Sprintf("%d: %s", c.OrderID, c.Reason))
		}
	})
	return p
}

// version puts each order under Core's versioned word at its status, as
// Core's first tell does: the same status, at version 1.
func (p *doorPin) version(ids ...int64) {
	p.t.Helper()
	for _, id := range ids {
		o := p.row(id)
		p.h.HandleOrderUpdate(wire, &protocol.OrderUpdate{OrderUUID: o.UUID, Status: string(o.Status), Version: 1})
		if v := p.row(id).CoreVersion; v != 1 {
			p.t.Fatalf("fixture: order %d reads version %d after Core's word, want 1", id, v)
		}
	}
}

func (p *doorPin) row(id int64) *storeorders.Order {
	p.t.Helper()
	o, err := p.db.GetOrder(id)
	testutil.MustNoErr(p.t, err, "read order")
	return o
}

// drain marks everything the Edge has queued as sent.
func (p *doorPin) drain() {
	p.t.Helper()
	msgs, err := p.db.ListPendingOutbox(1000)
	testutil.MustNoErr(p.t, err, "read the outbox")
	for _, m := range msgs {
		testutil.MustNoErr(p.t, p.db.AckOutbox(m.ID), "ack an outbox row")
	}
}

// sent is every order.cancel queued since the last drain, oldest first, each
// as the JSON object of its payload: a key a build adds shows.
func (p *doorPin) sent() []map[string]any {
	p.t.Helper()
	msgs, err := p.db.ListUnsentOutboxByType([]string{protocol.TypeOrderCancel})
	testutil.MustNoErr(p.t, err, "read the outbox")
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		var env protocol.Envelope
		testutil.MustNoErr(p.t, json.Unmarshal(m.Payload, &env), "decode an envelope")
		var body map[string]any
		testutil.MustNoErr(p.t, json.Unmarshal(env.Payload, &body), "decode an order.cancel")
		out = append(out, body)
	}
	return out
}

// cancelOf is one order.cancel as the Edge sends it today: no actor for a
// person's door, protocol.CancelActorEdge for the Edge's own rule.
func cancelOf(uuid, reason, actor string) map[string]any {
	m := map[string]any{"order_uuid": uuid, "reason": reason}
	if actor != "" {
		m["actor"] = actor
	}
	return m
}

// expectSent fails unless the Edge queued exactly want.
func (p *doorPin) expectSent(step string, want ...map[string]any) {
	p.t.Helper()
	got := p.sent()
	if len(got) != len(want) {
		p.t.Errorf("%s: the Edge queued %v, want %v", step, got, want)
		return
	}
	for i := range want {
		if fmt.Sprint(got[i]) != fmt.Sprint(want[i]) {
			p.t.Errorf("%s: order.cancel %d is %v, want %v", step, i, got[i], want[i])
		}
	}
}

// tile is the tile's orders in the station JSON, "id status" plus each key
// the board reads beyond the row's identity that a cancel could set (none
// today): the board's own query (ListActiveByNodeKeys) marshalled as a tile
// marshals its orders (StationNodeView.MarshalJSON).
func (p *doorPin) tile(nodeID int64, coreNode string) []string {
	p.t.Helper()
	byNode, err := storeorders.ListActiveByNodeKeys(p.db.DB, []storeorders.NodeKey{{ProcessNodeID: nodeID, CoreNodeName: coreNode}})
	testutil.MustNoErr(p.t, err, "the board's orders")
	raw, err := json.Marshal(domain.StationNodeView{Orders: byNode[nodeID]})
	testutil.MustNoErr(p.t, err, "marshal the tile")
	var view struct {
		Orders []map[string]any `json:"orders"`
	}
	testutil.MustNoErr(p.t, json.Unmarshal(raw, &view), "decode the tile")
	var out []string
	for _, o := range view.Orders {
		line := fmt.Sprintf("%v %v", o["id"], o["status"])
		for _, k := range []string{"cancel_sent", "cancel_refused"} {
			if v, ok := o[k]; ok {
				line += fmt.Sprintf(" %s=%v", k, v)
			}
		}
		out = append(out, line)
	}
	sort.Strings(out)
	return out
}

// coreCancels plays Core's end of a station's cancel of order id: its
// order.update cancelled at the next version, with the reason as its detail,
// then its order.cancelled naming the status Core held.
func (p *doorPin) coreCancels(id int64, reason string, previous protocol.Status) {
	p.t.Helper()
	o := p.row(id)
	p.h.HandleOrderUpdate(wire, &protocol.OrderUpdate{OrderUUID: o.UUID, Status: string(protocol.StatusCancelled),
		Detail: reason, Version: o.CoreVersion + 1})
	p.h.HandleOrderCancelled(wire, &protocol.OrderCancelled{OrderUUID: o.UUID, Reason: reason,
		PreviousStatus: string(previous)})
}

// history is the order's history rows, "old->new: detail", in write order.
func (p *doorPin) history(id int64) []string {
	p.t.Helper()
	hs, err := p.db.ListOrderHistory(id)
	testutil.MustNoErr(p.t, err, "read the history")
	sort.Slice(hs, func(i, j int) bool { return hs[i].ID < hs[j].ID })
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, string(h.OldStatus)+"->"+string(h.NewStatus)+": "+h.Detail)
	}
	return out
}

// historySince is the order's history rows after the first n.
func (p *doorPin) historySince(id int64, n int) []string {
	p.t.Helper()
	h := p.history(id)
	if n > len(h) {
		return nil
	}
	return h[n:]
}

func expectLines(t *testing.T, step string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s: %q, want %q", step, got, want)
	}
}

// TestCancelDoorPin_Abandon: ABANDON of a changeover supply parked for lack of
// material (AbandonChangeoverSupply, plain). The supply's cancel goes with its
// ordinary reason, the task lands abandoned with ABANDON's note, and the
// cutover is poked. Today the supply ends at the tap, before Core hears, and
// Core's word moves nothing on the ended row.
func TestCancelDoorPin_Abandon(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	p := newDoorPin(t, db, eng)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenarioMode(t, db, protocol.SwapModeTwoRobot)
	_, task := startChangeover(t, eng, db, processID, toStyleID)
	if task.NextMaterialOrderID == nil || task.OldMaterialReleaseOrderID == nil {
		t.Fatal("fixture: the task needs a supply and an evac")
	}
	supplyID, evacID := *task.NextMaterialOrderID, *task.OldMaterialReleaseOrderID
	testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(orders.StatusQueued)), "park the supply")
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(orders.StatusQueued)), "the evac queued")
	testutil.MustNoErr(t, db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskAwaitingMaterial), "awaiting material")
	p.version(supplyID, evacID)
	p.drain()
	supply := p.row(supplyID)
	before := len(p.history(supplyID))
	const reason = "changeover supply abandoned by operator"
	const note = "supply abandoned: material unavailable at Phase3 Swap Node"

	testutil.MustNoErr(t, eng.AbandonChangeoverSupply(processID, nodeID, false, "test-operator"), "abandon")

	// After the tap: the supply has ended, and its tile lists the evac alone.
	expectLines(t, "the tile after the tap", p.tile(nodeID, "P3-NODE"), fmt.Sprintf("%d queued", evacID))
	p.expectSent("the tap", cancelOf(supply.UUID, reason, ""))
	expectLines(t, "the supply's rows at the tap", p.historySince(supplyID, before), "queued->cancelled: "+reason)
	tk, err := db.GetChangeoverNodeTaskByNode(task.ProcessChangeoverID, nodeID)
	testutil.MustNoErr(t, err, "read the task")
	if tk.State != domain.NodeTaskAbandoned || tk.SkipNote != note {
		t.Errorf("the task after the tap reads %s %q, want abandoned %q", tk.State, tk.SkipNote, note)
	}

	// Core's end: its word lands on the ended row and moves nothing.
	p.coreCancels(supplyID, reason, protocol.StatusQueued)
	if s := p.row(supplyID).Status; s != orders.StatusCancelled {
		t.Errorf("after Core's word the supply reads %s, want cancelled", s)
	}
	expectLines(t, "the supply's rows at the end", p.historySince(supplyID, before), "queued->cancelled: "+reason)
	expectLines(t, "the toasts", p.toasts)
	tk, err = db.GetChangeoverNodeTaskByNode(task.ProcessChangeoverID, nodeID)
	testutil.MustNoErr(t, err, "read the task")
	if tk.State != domain.NodeTaskAbandoned || tk.SkipNote != note {
		t.Errorf("the task at the end reads %s %q, want abandoned %q", tk.State, tk.SkipNote, note)
	}
	if s := p.row(evacID).Status; s != orders.StatusQueued {
		t.Errorf("the evac reads %s, want queued: Core cancels the partner, the Edge does not", s)
	}
}

// TestCancelDoorPin_OrdersPage: the Orders page's Cancel (www apiCancelOrder:
// Manager.CancelOrderByPerson, its one call) of an order with a robot on it.
// Its own work is the reorder hold's: a pause on the position, so the sweep
// does not ask again a minute after a person said stop. Today the order ends
// at the tap, its completion clears the position's order slot, and the pause
// is written at the tap; Core's word moves nothing on the ended row.
func TestCancelDoorPin_OrdersPage(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	p := newDoorPin(t, db, eng)
	_, nodeID := seedProcessNode(t, db)
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "the position's runtime row")
	id, err := db.CreateOrder("door-page", orders.TypeRetrieve, &nodeID, false, 1, "TEST-NODE", "", "SRC",
		protocol.Height{}, false, "PART-M", "", "")
	testutil.MustNoErr(t, err, "create the order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(protocol.StatusInTransit)), "a robot carries it")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &id, nil), "the position's order slot")
	p.version(id)
	p.drain()
	o := p.row(id)
	before := len(p.history(id))
	note := fmt.Sprintf("a person cancelled order %d", id)

	testutil.MustNoErr(t, eng.orderMgr.CancelOrderByPerson(id), "the person's cancel")

	expectLines(t, "the tile after the tap", p.tile(nodeID, "TEST-NODE"))
	p.expectSent("the tap", cancelOf(o.UUID, "cancelled by operator", ""))
	expectLines(t, "the rows at the tap", p.historySince(id, before), "in_transit->cancelled: cancelled by operator")
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read the runtime row")
	if rt.ActiveOrderID != nil || rt.ReorderHold != domain.ReorderHoldPaused || rt.ReorderHoldNote != note {
		t.Errorf("after the tap the position holds order %v, %q %q; want no order, paused %q", rt.ActiveOrderID,
			rt.ReorderHold, rt.ReorderHoldNote, note)
	}

	p.coreCancels(id, "cancelled by operator", protocol.StatusInTransit)
	if s := p.row(id).Status; s != orders.StatusCancelled {
		t.Errorf("after Core's word the order reads %s, want cancelled", s)
	}
	expectLines(t, "the rows at the end", p.historySince(id, before), "in_transit->cancelled: cancelled by operator")
	expectLines(t, "the toasts", p.toasts)
	rt, err = db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read the runtime row")
	if rt.ReorderHold != domain.ReorderHoldPaused || rt.ReorderHoldNote != note {
		t.Errorf("at the end the position holds %q %q, want paused %q", rt.ReorderHold, rt.ReorderHoldNote, note)
	}
}

// TestCancelDoorPin_IdleRule: the idle rule sends back a swap pair parked at
// a line silent for its hour (idle_cancel.go, CancelOrderAsEdge): off at the
// position first, then each leg's cancel with the rule's reason and the
// Edge's actor. Its own work in the next pass: nothing, the pair is gone.
// Today both legs end at the tap; Core's word moves nothing on the ended rows.
func TestCancelDoorPin_IdleRule(t *testing.T) {
	t.Parallel()
	l := newIdleLine(t, 61*time.Minute)
	p := newDoorPin(t, l.db, l.eng)
	ids := l.addLegs(t, parked, parked)
	p.version(ids...)
	p.drain()
	var want []map[string]any
	for _, id := range ids {
		want = append(want, cancelOf(p.row(id).UUID, protocol.CancelReasonLineIdle, protocol.CancelActorEdge))
	}

	l.eng.reconcileDemand()

	expectLines(t, "the tile after the pass", p.tile(l.nodeID, "ALN_IDLE"))
	p.expectSent("the pass", want...)
	for _, id := range ids {
		expectLines(t, fmt.Sprintf("leg %d's rows at the pass", id), p.history(id),
			"staged->cancelled: "+protocol.CancelReasonLineIdle)
	}
	if got := l.hold(t); got != domain.ReorderHoldOff {
		t.Errorf("the position holds %q after the pass, want off", got)
	}
	p.drain()
	l.eng.reconcileDemand()
	p.expectSent("the next pass")

	for _, id := range ids {
		p.coreCancels(id, protocol.CancelReasonLineIdle, protocol.StatusStaged)
		expectLines(t, fmt.Sprintf("leg %d's rows at the end", id), p.history(id),
			"staged->cancelled: "+protocol.CancelReasonLineIdle)
	}
	expectLines(t, "the toasts", p.toasts)
	if got := countOrders(t, l.db); got != 2 {
		t.Errorf("orders at the end = %d, want the two legs: nothing asked again at an off position", got)
	}
}

// TestCancelDoorPin_CancelSendBack: CANCEL on a press whose SEND BACK has not
// moved (CancelSendBack): both of SEND BACK's orders cancelled with its
// reason, and the count of them is what the tap answers. Today both end at
// the tap; Core's word moves nothing on the ended rows.
func TestCancelDoorPin_CancelSendBack(t *testing.T) {
	t.Parallel()
	c := seedSendBackPress(t, testEngineDB(t))
	p := newDoorPin(t, c.db, c.eng)
	res, err := c.eng.SendBack(c.press)
	testutil.MustNoErr(t, err, "SEND BACK")
	ids := []int64{res.Move.ID, res.Pull.ID}
	p.version(ids...)
	p.drain()
	press, err := c.db.GetProcessNode(c.press)
	testutil.MustNoErr(t, err, "the press")
	reason := "SEND BACK cancelled at " + press.Name
	var want []map[string]any
	statusAt := map[int64]protocol.Status{}
	for _, id := range ids {
		want = append(want, cancelOf(p.row(id).UUID, reason, ""))
		statusAt[id] = p.row(id).Status
	}

	n, err := c.eng.CancelSendBack(c.press)
	testutil.MustNoErr(t, err, "CANCEL")

	if n != 2 {
		t.Errorf("CANCEL answered %d, want both of SEND BACK's orders", n)
	}
	outside, err := c.db.GetProcessNode(c.outside)
	testutil.MustNoErr(t, err, "the outside")
	expectLines(t, "the outside's tile after the tap", p.tile(c.outside, outside.CoreNodeName))
	p.expectSent("the tap", want...)
	for _, id := range ids {
		if s := p.row(id).Status; s != orders.StatusCancelled {
			t.Errorf("order %d reads %s after the tap, want cancelled", id, s)
		}
		p.coreCancels(id, reason, statusAt[id])
		if s := p.row(id).Status; s != orders.StatusCancelled {
			t.Errorf("order %d reads %s at the end, want cancelled", id, s)
		}
	}
	expectLines(t, "the toasts", p.toasts)
}

// TestCancelDoorPin_StartCancels: a changeover START cancels the order not yet
// with the fleet at a node the changeover acts on (startCancel.abort), clears
// the slot it held, and goes on to create its legs. Today the order ends at
// the tap; Core's word moves nothing on the ended row.
func TestCancelDoorPin_StartCancels(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	p := newDoorPin(t, db, eng)
	id := seedOrderAt(t, db, nodeID, "CO-NODE", "door-start", "PART-OLD", orders.StatusQueued)
	p.version(id)
	p.drain()
	o := p.row(id)

	co, err := eng.StartProcessChangeover(processID, toStyleID, "test", "")
	testutil.MustNoErr(t, err, "START")

	if s := p.row(id).Status; s != orders.StatusCancelled {
		t.Errorf("the queued order reads %s after START, want cancelled", s)
	}
	sent := p.sent()
	first := cancelOf(o.UUID, changeoverStartCancelReason, "")
	if len(sent) == 0 || fmt.Sprint(sent[0]) != fmt.Sprint(first) {
		t.Errorf("START queued %v first, want the queued order's cancel %v", sent, first)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read the runtime row")
	if rt.ActiveOrderID != nil && *rt.ActiveOrderID == id {
		t.Errorf("the position's slot still names the cancelled order %d", id)
	}
	tasks, err := db.ListChangeoverNodeTasks(co.ID)
	testutil.MustNoErr(t, err, "the tasks")
	if len(tasks) != 1 {
		t.Errorf("START made %d tasks, want its one: it goes on past its cancel", len(tasks))
	}

	p.coreCancels(id, changeoverStartCancelReason, orders.StatusQueued)
	if s := p.row(id).Status; s != orders.StatusCancelled {
		t.Errorf("the order reads %s at the end, want cancelled", s)
	}
	expectLines(t, "the toasts", p.toasts)
}

// TestCancelDoorPin_ChangeoverCancel: a changeover's CANCEL with its swap's
// two legs live (cancelProcessChangeoverInternal: the task cancelled first,
// then taskLeftOnStaging's abort of each leg). Its own work: the position's
// slots cleared, the changeover row cancelled, the style back; and with a
// redirect, the next changeover started at once. Today the legs end at the
// tap; Core's word moves nothing on the ended rows.
func TestCancelDoorPin_ChangeoverCancel(t *testing.T) {
	t.Parallel()
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprintf("redirect=%v", redirect), func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			p := newDoorPin(t, db, eng)
			processID, nodeID, fromStyleID, toStyleID := seedPhase3SwapScenarioMode(t, db, protocol.SwapModeTwoRobot)
			var thirdID int64
			if redirect {
				var err error
				thirdID, err = db.CreateStyle("Style-P3-THIRD", "the redirect's style", processID)
				testutil.MustNoErr(t, err, "the third style")
				_, err = upsertClaimRetiredMode(db, processes.NodeClaimInput{
					StyleID: thirdID, CoreNodeName: "P3-NODE", Role: "consume", SwapMode: protocol.SwapModeTwoRobot,
					PayloadCode: "PART-THIRD", UOPCapacity: 100, InboundSource: "SOURCE-THIRD",
					InboundStaging: "IN-STAGING", OutboundStaging: "OUT-STAGING", OutboundDestination: "DEST-THIRD",
				})
				testutil.MustNoErr(t, err, "the third claim")
			}
			co, task := startChangeover(t, eng, db, processID, toStyleID)
			supplyID, evacID := *task.NextMaterialOrderID, *task.OldMaterialReleaseOrderID
			testutil.MustNoErr(t, db.UpdateOrderStatus(supplyID, string(protocol.StatusInTransit)), "the supply drives")
			testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(protocol.StatusStaged)), "the evac waits")
			p.version(supplyID, evacID)
			p.drain()
			want := []map[string]any{cancelOf(p.row(supplyID).UUID, "cancelled by operator", ""),
				cancelOf(p.row(evacID).UUID, "cancelled by operator", "")}

			if redirect {
				testutil.MustNoErr(t, eng.CancelProcessChangeoverRedirect(processID, &thirdID), "CANCEL and redirect")
			} else {
				testutil.MustNoErr(t, eng.CancelProcessChangeover(processID), "CANCEL")
			}

			for _, id := range []int64{supplyID, evacID} {
				if s := p.row(id).Status; s != orders.StatusCancelled {
					t.Errorf("leg %d reads %s after the tap, want cancelled", id, s)
				}
			}
			sent := p.sent()
			if len(sent) < 2 || fmt.Sprint(sent[:2]) != fmt.Sprint(want) {
				t.Errorf("the CANCEL queued %v first, want the legs' cancels %v", sent, want)
			}
			old, err := db.GetProcessChangeoverByID(co.ID)
			testutil.MustNoErr(t, err, "the cancelled changeover")
			if old.State != domain.ChangeoverCancelled {
				t.Errorf("the changeover reads %s, want cancelled", old.State)
			}
			proc, err := db.GetProcess(processID)
			testutil.MustNoErr(t, err, "the process")
			if proc.ActiveStyleID == nil || *proc.ActiveStyleID != fromStyleID {
				t.Errorf("the process runs style %v, want the outgoing style %d back", proc.ActiveStyleID, fromStyleID)
			}
			next, err := db.GetActiveProcessChangeover(processID)
			if redirect && (err != nil || next == nil || next.ToStyleID != thirdID) {
				t.Errorf("after CANCEL and redirect the open changeover is %+v (%v), want one to style %d", next, err, thirdID)
			}
			if !redirect && err == nil && next != nil {
				t.Errorf("after CANCEL a changeover is open: %+v", next)
			}
			rt, err := db.GetProcessNodeRuntime(nodeID)
			testutil.MustNoErr(t, err, "read the runtime row")
			for _, slot := range []*int64{rt.ActiveOrderID, rt.StagedOrderID} {
				if slot != nil && (*slot == supplyID || *slot == evacID) {
					t.Errorf("the position's slot still names leg %d", *slot)
				}
			}

			p.coreCancels(supplyID, "cancelled by operator", protocol.StatusInTransit)
			p.coreCancels(evacID, "cancelled by operator", protocol.StatusStaged)
			for _, id := range []int64{supplyID, evacID} {
				if s := p.row(id).Status; s != orders.StatusCancelled {
					t.Errorf("leg %d reads %s at the end, want cancelled", id, s)
				}
			}
			expectLines(t, "the toasts", p.toasts)
		})
	}
}

// TestCancelDoorPin_KeepStagedSpotCancels: a keep-staged line's changeover
// CANCEL (abortSpotOrdersNotFlown): the spot's return the start made and no
// robot has taken is cancelled, so the staying style keeps its empty, and the
// cancel orders nothing for the spot. With the return already flying it is
// left to land, and the spot is the keeper's at its next decision point: the
// cancel's own aborts' passes are turned away under its cell locks and
// dropped as it lets go (letGoDecided). Today the spot's and the legs' orders
// end at the tap; Core's word moves nothing on the ended rows.
func TestCancelDoorPin_KeepStagedSpotCancels(t *testing.T) {
	t.Parallel()
	t.Run("the return not flown is cancelled", func(t *testing.T) {
		t.Parallel()
		fx := seedLanding(t, protocol.ClaimRoleProduce,
			map[string]protocol.NodeBinInfo{"L1": {Occupied: true}, "SPOT": {Occupied: true, BinTypeCode: "TYPE-A"}})
		fx.eng.SetPayloadBinTypes(ksCatalog(map[string]string{"PART-OLD": "TYPE-A", "PART-NEW": "TYPE-B"}))
		p := adoptDoorPin(t, fx.db, fx.eng)
		startKSChangeover(t, fx)
		_, returns := liveSpot(t, fx, "SPOT")
		if len(returns) != 1 {
			t.Fatalf("fixture: the start made %d returns of the outgoing empty, want 1", len(returns))
		}
		ret := returns[0]
		live, err := fx.db.ListActiveOrders()
		testutil.MustNoErr(t, err, "the live orders")
		var ids []int64
		for _, o := range live {
			ids = append(ids, o.ID)
		}
		p.version(ids...)
		p.drain()

		testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "CANCEL")

		if s := p.row(ret.ID).Status; s != orders.StatusCancelled {
			t.Errorf("the spot's return reads %s after the tap, want cancelled", s)
		}
		sent := p.sent()
		spotCancel := cancelOf(ret.UUID, "changeover cancelled: the keep-staged spot goes back to the outgoing style", "")
		if len(sent) == 0 || fmt.Sprint(sent[0]) != fmt.Sprint(spotCancel) {
			t.Errorf("the CANCEL queued %v first, want the spot's return's cancel %v", sent, spotCancel)
		}
		if refills, returns := liveSpot(t, fx, "SPOT"); len(refills) != 0 || len(returns) != 0 {
			t.Errorf("after the CANCEL the spot has %d refills and %d returns live, want none", len(refills), len(returns))
		}
		for _, id := range ids {
			p.coreCancels(id, "cancelled", protocol.StatusQueued)
		}
		if refills, returns := liveSpot(t, fx, "SPOT"); len(refills) != 0 || len(returns) != 0 {
			t.Errorf("after Core's word the spot has %d refills and %d returns live, want none", len(refills), len(returns))
		}
		expectLines(t, "the toasts", p.toasts)
	})
	t.Run("the return flying is left, and the keeper waits", func(t *testing.T) {
		t.Parallel()
		fx := seedLanding(t, protocol.ClaimRoleConsume, map[string]protocol.NodeBinInfo{
			"L1": {Occupied: true, PayloadCode: "PART-OLD"}, "SPOT": {Occupied: true, PayloadCode: "PART-OLD"}})
		p := adoptDoorPin(t, fx.db, fx.eng)
		startKSChangeover(t, fx)
		_, returns := liveSpot(t, fx, "SPOT")
		if len(returns) != 1 {
			t.Fatalf("fixture: the start made %d returns, want 1", len(returns))
		}
		testutil.MustNoErr(t, fx.db.UpdateOrderStatus(returns[0].ID, string(protocol.StatusInTransit)), "the return flies")
		live, err := fx.db.ListActiveOrders()
		testutil.MustNoErr(t, err, "the live orders")
		var legs []int64
		for _, o := range live {
			if o.ID != returns[0].ID {
				legs = append(legs, o.ID)
			}
		}
		p.version(append([]int64{returns[0].ID}, legs...)...)
		p.drain()

		testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "CANCEL")
		for _, id := range legs {
			p.coreCancels(id, "cancelled by operator", protocol.StatusQueued)
		}

		refills, returns := liveSpot(t, fx, "SPOT")
		if len(returns) != 1 || len(refills) != 0 {
			t.Errorf("after the CANCEL and Core's word: %d returns and %d refills live, want the one flying and "+
				"no refill: the spot is the keeper's at its next decision point", len(returns), len(refills))
		}
		fx.eng.sweepCellLevels()
		refills, _ = liveSpot(t, fx, "SPOT")
		if len(refills) != 1 {
			t.Errorf("after the keeper's pass %d refills, want 1 to stand behind the spare that is leaving", len(refills))
		}
		expectLines(t, "the toasts", p.toasts)
	})
}
