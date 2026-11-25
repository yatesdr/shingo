//go:build docker

package scenarios

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/debuglog"

	"shingocore/dispatch"
	corebins "shingocore/store/bins"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
	edgeuop "shingoedge/uop"
	edgewww "shingoedge/www"
)

// delivered_order_test.go: brief D item 2 (docket R92), data only.
//
// CHARACTERISATION FOR BRIEF D ITEM 2, NOT A RULING. Two doors that cancel an
// order that is no dig leg, each run while a line delivery is delivered and
// not yet confirmed (waiting on its receipt). Brief C recorded the six dig
// doors on legs (shingo-core dispatch/delivered_leg_docker_test.go,
// engine/delivered_leg_clear_docker_test.go); these record the two outside the
// dig, across both modules, because the Edge's row and the operator's screen
// are part of what each door does. Each asserts what happens today, even where
// it looks wrong: the order's end, its bin's row and claim, what reaches the
// fleet and the station, the Edge's row, the line's tile on the station's page
// and whether anything says what happened. The orc and the owner rule on it; a
// change that fails one of these changes the behaviour it records, and the
// test is re-read against that ruling, never loosened to pass.
//
// The cell is the keep-staged recovery cell (newKsrCell) on a consume line in
// two_robot with no staged spare: Core's engine on the fleet simulator, the
// Edge's engine, the bus between them, Core's bins feed and its landings. Each
// test adds what dloRig adds: the seat's count writer (the harness leaves it
// unwired), the line on an operator station and the Edge's own web server, so
// the tile is read as the board fetches it. What the board then draws from the
// tile (CONFIRM for a delivered order that needs a receipt, www
// static/operator-station/operator-modal.js cellCardAction) is read from the
// page's code, not rendered.

// dloTile is the line's tile on the station's page as GET
// /api/operator-stations/{id}/view serves it (www apiGetOperatorStationView,
// StationNodeView's JSON): the keys the board draws the tile from.
type dloTile struct {
	Node struct {
		ID int64 `json:"id"`
	} `json:"node"`
	Runtime *struct {
		ActiveBinID        *int64 `json:"active_bin_id"`
		RemainingUOPCached int    `json:"remaining_uop_cached"`
	} `json:"runtime"`
	Orders []struct {
		ID          int64  `json:"id"`
		Status      string `json:"status"`
		AutoConfirm bool   `json:"auto_confirm"`
		Departed    bool   `json:"departed"`
	} `json:"orders"`
	BinState *struct {
		BinID    int64  `json:"bin_id"`
		BinLabel string `json:"bin_label"`
		Stocked  bool   `json:"stocked"`
		Occupied bool   `json:"occupied"`
	} `json:"bin_state"`
	ReleasesAsPair  bool   `json:"releases_as_pair"`
	AutoReorderNote string `json:"auto_reorder_note"`
	AutoReorderChip string `json:"auto_reorder_chip"`
}

// seat is the tile's bound bin (0 for none) and its count.
func (t dloTile) seat() (int64, int) {
	if t.Runtime == nil {
		return 0, 0
	}
	var bin int64
	if t.Runtime.ActiveBinID != nil {
		bin = *t.Runtime.ActiveBinID
	}
	return bin, t.Runtime.RemainingUOPCached
}

// lists reports whether the tile draws the order, at the status.
func (t dloTile) lists(id int64, status protocol.Status) bool {
	for _, o := range t.Orders {
		if o.ID == id && o.Status == string(status) {
			return true
		}
	}
	return false
}

func (t dloTile) String() string {
	var orders []string
	for _, o := range t.Orders {
		orders = append(orders, fmt.Sprintf("%d %s auto_confirm=%v departed=%v", o.ID, o.Status, o.AutoConfirm, o.Departed))
	}
	bin, count := t.seat()
	state := "none"
	if b := t.BinState; b != nil {
		state = fmt.Sprintf("%s (bin %d) stocked=%v occupied=%v", b.BinLabel, b.BinID, b.Stocked, b.Occupied)
	}
	return fmt.Sprintf("orders=%v seat=bin %d count %d bin_state=%s releases_as_pair=%v auto_reorder_note=%q chip=%q",
		orders, bin, count, state, t.ReleasesAsPair, t.AutoReorderNote, t.AutoReorderChip)
}

// dloTell is one message Core sent the station about an order, as it crossed
// the bus.
type dloTell struct {
	Type      string
	OrderUUID string `json:"order_uuid"`
	Status    string `json:"status"`
	Detail    string `json:"detail"`
	Reason    string `json:"reason"`
}

func (m dloTell) String() string {
	return fmt.Sprintf("%s status=%q detail=%q reason=%q", m.Type, m.Status, m.Detail, m.Reason)
}

// dloRig is what both doors add to the cell: the seat's count writer, which
// the harness leaves unwired and without which no landing binds the line's
// seat (handler_bin_landed.go landCarrier; wired as main.go wires it, as
// send_back_test.go does); the line on an operator station; the Edge's own web
// server (www.NewRouter, as edge_debug_page_test.go serves it) for the
// station's page; what Core told the station (every Core → Edge message on an
// order, read off the bus) and what the Edge announced as cancelled by Core
// (EventOrderCancelledByCore, the station's sticky toast).
type dloRig struct {
	c       *ksrCell
	station int64
	page    string

	mu     sync.Mutex
	told   []dloTell
	byCore []edgeengine.OrderCancelledByCoreEvent
}

func newDloRig(c *ksrCell) *dloRig {
	c.t.Helper()
	r := &dloRig{c: c}
	db := c.edge.DB
	c.edge.Engine.SetInventoryDeltaSink(edgeuop.New(db, "edge.test", db, db))
	var err error
	r.station, err = db.CreateOperatorStation(stations.Input{ProcessID: c.processID, Code: "DLO-ST",
		Name: "DLO Station", Sequence: 1, Enabled: true})
	mustNil(c.t, err, "the line's operator station")
	mustNil(c.t, db.UpdateProcessNode(c.nodeID, processes.NodeInput{ProcessID: c.processID, OperatorStationID: &r.station,
		CoreNodeName: ksrLine, Code: "KSR1", Name: ksrLine, Sequence: 1, Enabled: true}), "the line on its station")
	dbg, err := debuglog.New(64, nil)
	mustNil(c.t, err, "debug log")
	_, handler, stop := edgewww.NewRouter(c.edge.Engine, dbg, nil)
	c.t.Cleanup(stop)
	srv := httptest.NewServer(handler)
	c.t.Cleanup(srv.Close)
	r.page = srv.URL
	c.bus.RewriteCoreToEdge(func(raw []byte) []byte {
		var env protocol.Envelope
		if json.Unmarshal(raw, &env) == nil && env.Type != protocol.TypeData {
			var m dloTell
			if json.Unmarshal(env.Payload, &m) == nil && m.OrderUUID != "" {
				m.Type = env.Type
				r.mu.Lock()
				r.told = append(r.told, m)
				r.mu.Unlock()
			}
		}
		return raw
	})
	c.edge.Engine.Events.SubscribeTypes(func(evt edgeengine.Event) {
		if p, ok := evt.Payload.(edgeengine.OrderCancelledByCoreEvent); ok {
			r.mu.Lock()
			r.byCore = append(r.byCore, p)
			r.mu.Unlock()
		}
	}, edgeengine.EventOrderCancelledByCore)
	return r
}

// dloMark is where a door is measured from: the records so far.
type dloMark struct{ told, byCore int }

func (r *dloRig) mark() dloMark {
	r.mu.Lock()
	defer r.mu.Unlock()
	return dloMark{told: len(r.told), byCore: len(r.byCore)}
}

// toldSince is what Core told the station about the order with edge uuid
// uuid since m.
func (r *dloRig) toldSince(m dloMark, uuid string) []dloTell {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []dloTell
	for _, t := range r.told[m.told:] {
		if t.OrderUUID == uuid {
			out = append(out, t)
		}
	}
	return out
}

// announcedSince is what the Edge announced as cancelled by Core since m.
func (r *dloRig) announcedSince(m dloMark) []edgeengine.OrderCancelledByCoreEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.byCore[m.byCore:])
}

// tile is the line's tile, fetched from the station's page.
func (r *dloRig) tile() dloTile {
	r.c.t.Helper()
	res, err := http.Get(fmt.Sprintf("%s/api/operator-stations/%d/view", r.page, r.station))
	mustNil(r.c.t, err, "GET the station's view")
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	mustNil(r.c.t, err, "read the station's view")
	if res.StatusCode != http.StatusOK {
		r.c.t.Fatalf("GET the station's view: %d %s", res.StatusCode, body)
	}
	var view struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	mustNil(r.c.t, json.Unmarshal(body, &view), "decode the station's view")
	for _, raw := range view.Nodes {
		var t dloTile
		mustNil(r.c.t, json.Unmarshal(raw, &t), "decode a tile")
		if t.Node.ID == r.c.nodeID {
			return t
		}
	}
	r.c.t.Fatalf("the station's view has no tile for the line (node %d): %s", r.c.nodeID, body)
	return dloTile{}
}

// cart is the bin's row at Core, and whether Core's sourcing predicate
// (store/bins.BinSourceableSQL, which every full-bin finder composes) would
// offer it to an order.
func (r *dloRig) cart(id int64) (*corebins.Bin, bool) {
	r.c.t.Helper()
	db := r.c.core.eng.DB()
	b, err := db.GetBin(id)
	mustNil(r.c.t, err, "read the bin")
	var n int
	mustNil(r.c.t, db.QueryRow(`SELECT count(*) `+corebins.BinFromClause+` WHERE `+corebins.BinSourceableSQL("$1")+
		` AND b.id = $2`, b.PayloadCode, b.ID).Scan(&n), "ask the sourcing predicate of the bin")
	return b, n == 1
}

// plantWidePick is the bin Core's finder would hand an order for a full of
// part with no source named (ForecastPick: the finder, nothing written), or
// nil when it finds none.
func (r *dloRig) plantWidePick(part string) *corebins.Bin {
	res := r.c.core.eng.Dispatcher().ForecastPick(dispatch.SourceNeed{PayloadCode: part, Intent: dispatch.IntentFull,
		DeliveryNode: ksrOutStg})
	if res.Outcome != dispatch.OutcomeFound {
		return nil
	}
	return res.Bin
}

// audits lists the actions of the order's audit rows at Core, oldest first.
func (r *dloRig) audits(orderID int64) []string {
	r.c.t.Helper()
	rows, err := r.c.core.eng.DB().Query(`SELECT action FROM audit_log WHERE entity_type = 'order' AND entity_id = $1
		ORDER BY id`, orderID)
	mustNil(r.c.t, err, "read the order's audit")
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		mustNil(r.c.t, rows.Scan(&a), "scan an audit row")
		out = append(out, a)
	}
	mustNil(r.c.t, rows.Err(), "read the order's audit")
	return out
}

// ended is an order's last two statuses at Core and the code of its ending.
func (r *dloRig) ended(id int64) ([]protocol.Status, string) {
	r.c.t.Helper()
	hist, err := r.c.core.eng.DB().ListOrderHistory(id)
	mustNil(r.c.t, err, "read the order's history")
	var steps []protocol.Status
	code := ""
	for _, h := range hist {
		steps = append(steps, h.Status)
		code = h.Code
	}
	if len(steps) > 2 {
		steps = steps[len(steps)-2:]
	}
	return steps, code
}

// edgeEnding is the Edge's history of the order from its first cancelled row:
// each row's status and sentence.
func (r *dloRig) edgeEnding(id int64) []string {
	r.c.t.Helper()
	hist, err := r.c.edge.DB.ListOrderHistory(id)
	mustNil(r.c.t, err, "read the Edge's history")
	var out []string
	for _, h := range hist {
		if len(out) > 0 || h.NewStatus == protocol.StatusCancelled {
			out = append(out, fmt.Sprintf("%s %q", h.NewStatus, h.Detail))
		}
	}
	return out
}

// TestDeliveredOrder_SwapPeer: resolveSwapPeer (dispatch/swap_peer.go) on a
// delivered, unconfirmed partner. A steady-state two-robot swap on the line:
// REQUEST makes the evac and the supply, both robots park at their waits, the
// operator releases them, the evac lifts the line's bin and sets off for the
// outbound destination, and the supply sets the new bin down on the line. The
// supply places a bin on the line, so it is minted needing a receipt
// (auto_confirm false, engine confirmPolicy), and Core confirms no top-level
// order in line: it waits for CONFIRM or the reconciliation sweep. Then the
// fleet stops the evac's job (RDS STOPPED) with the old bin on its robot.
//
// Recorded today: Core cancels the evac with the fleet's words, and the
// swap-peer rule its cancel runs cancels the delivered supply, in a sentence
// that says the supply has yet to drop; the fleet is asked to stop the
// supply's finished job; the station is told the supply cancelled and the Edge
// ends its row; the tile no longer lists the supply, so the board draws no
// CONFIRM for it; the new bin stays on the line, unclaimed, still the seat's
// bin with its count, and Core's sourcing predicate refuses it; nothing names
// the half-swap: no swap_half_completed audit, and the Edge announces the
// evac's cancel alone.
func TestDeliveredOrder_SwapPeer(t *testing.T) {
	t.Parallel()
	c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeTwoRobot, plain: true, bareSpot: true})
	r := newDloRig(c)

	before := c.lastID()
	c.operatorRequest()
	c.tick()
	var evac, supply domain.Order
	for _, o := range c.newRows(before) {
		switch {
		case o.OrderType != protocol.OrderTypeComplex:
		case o.DeliveryNode == ksrLine:
			supply = o
		default:
			evac = o
		}
	}
	if evac.ID == 0 || supply.ID == 0 {
		c.dump("the swap")
		t.Fatalf("fixture: REQUEST made evac %d and supply %d, want both legs of a two-robot swap", evac.ID, supply.ID)
	}
	c.driveUntil("the swap's legs with the fleet", func() bool {
		c.fleetStep()
		return c.coreOf(evac).VendorOrderID != "" && c.coreOf(supply).VendorOrderID != ""
	})
	for _, l := range []domain.Order{evac, supply} {
		c.drive(c.coreOf(l), "RUNNING", "WAITING")
	}
	c.eventually("both legs staged at the Edge", func() bool {
		return c.edgeRow(evac.ID).Status == protocol.StatusStaged && c.edgeRow(supply.ID).Status == protocol.StatusStaged
	})
	mustNil(t, c.edge.Engine.ReleaseStagedOrders(c.nodeID, edgeengine.ReleaseDisposition{CalledBy: "dlo-operator"}),
		"release the swap")
	c.settle()
	c.drive(c.coreOf(evac), "RUNNING")
	if !c.blockDone(c.coreOf(evac), ksrLine, "JackLoad") {
		c.dump("the evac's lift")
		t.Fatal("fixture: the evac has no lift at the line")
	}
	c.drive(c.coreOf(supply), "RUNNING", "FINISHED")
	c.eventually("the supply delivered at both ends", func() bool {
		return c.coreOf(supply).Status == protocol.StatusDelivered && c.edgeRow(supply.ID).Status == protocol.StatusDelivered
	})
	sp, ev := c.coreOf(supply), c.coreOf(evac)
	newBin, oldBin := coreCart(sp), coreCart(ev)
	if newBin == nil || oldBin == nil || sp.CompletedAt != nil || c.edgeRow(supply.ID).AutoConfirm ||
		ev.Status != protocol.StatusInTransit {
		c.dump("the supply delivered")
		t.Fatalf("fixture: supply cart %v completed=%v auto_confirm=%v, evac cart %v %s; want the supply delivered "+
			"and waiting for its receipt, the evac carrying the old bin", newBin, sp.CompletedAt != nil,
			c.edgeRow(supply.ID).AutoConfirm, oldBin, ev.Status)
	}
	pre := r.tile()
	preSeat, preCount := pre.seat()
	t.Logf("before: supply %d delivered (bin %d), evac %d %s (bin %d); fleet %s; tile %s", sp.ID, *newBin, ev.ID,
		ev.Status, *oldBin, c.vendorState(sp.VendorOrderID), pre)
	if !pre.lists(supply.ID, protocol.StatusDelivered) || c.vendorState(sp.VendorOrderID) != "FINISHED" || preSeat != *newBin {
		t.Fatalf("fixture: the tile lists the supply delivered %v, its job reads %q, the seat holds bin %d; want "+
			"delivered, FINISHED and the new bin %d bound by its landing", pre.lists(supply.ID, protocol.StatusDelivered),
			c.vendorState(sp.VendorOrderID), preSeat, *newBin)
	}

	m := r.mark()
	c.drive(c.coreOf(evac), "STOPPED") // the fleet stops the evac's job
	c.settle()

	ev = c.coreOf(evac)
	ob, _ := r.cart(*oldBin)
	t.Logf("record: evac %d %s (%q); its bin %s (%d) at %s claimed_by=%v", ev.ID, ev.Status, ev.ErrorDetail,
		ob.Label, ob.ID, ob.NodeName, ob.ClaimedBy)
	if ev.Status != protocol.StatusCancelled || ev.ErrorDetail != "fleet order stopped" {
		t.Errorf("the evac is %q (%q), want cancelled with the fleet's words", ev.Status, ev.ErrorDetail)
	}

	// The partner's end.
	sp = c.coreOf(supply)
	steps, code := r.ended(sp.ID)
	want := fmt.Sprintf("coordinated swap evac (order %d) cancelled (fleet order stopped); "+
		"cancelling supply so it cannot drop onto an un-cleared line", ev.ID)
	t.Logf("record: supply %d %s (%q) completed=%v last=%v code=%q", sp.ID, sp.Status, sp.ErrorDetail,
		sp.CompletedAt != nil, steps, code)
	if sp.Status != protocol.StatusCancelled || sp.ErrorDetail != want || sp.CompletedAt != nil {
		t.Errorf("the supply is %q (%q) completed=%v, want cancelled with %q", sp.Status, sp.ErrorDetail,
			sp.CompletedAt != nil, want)
	}
	if !slices.Equal(steps, []protocol.Status{protocol.StatusDelivered, protocol.StatusCancelled}) ||
		code != string(protocol.TermPeerTerminal) {
		t.Errorf("the supply's history ends %v coded %q, want delivered then cancelled, coded %s", steps, code,
			protocol.TermPeerTerminal)
	}

	// Its bin, and whether Core would hand it on.
	b, sourceable := r.cart(*newBin)
	t.Logf("record: bin %s (%d) at %s status=%s claimed_by=%v uop=%d sourceable=%v", b.Label, b.ID, b.NodeName,
		b.Status, b.ClaimedBy, b.UOPRemaining, sourceable)
	if b.NodeName != ksrLine || b.ClaimedBy != nil || sourceable {
		t.Errorf("the supply's bin is at %s claimed by %v sourceable=%v, want on the line, unclaimed and refused "+
			"by the sourcing predicate", b.NodeName, b.ClaimedBy, sourceable)
	}

	// The fleet and the station.
	if st := c.vendorState(sp.VendorOrderID); st != "STOPPED" {
		t.Errorf("the supply's finished job reads %q at the fleet, want STOPPED: Core stops the job of the order it cancels", st)
	}
	told := r.toldSince(m, supply.UUID)
	t.Logf("record: told the station about the supply: %v", told)
	if len(told) != 2 || told[0].Type != protocol.TypeOrderUpdate || told[0].Status != string(protocol.StatusCancelled) ||
		told[0].Detail != want || told[1].Type != protocol.TypeOrderCancelled || told[1].Reason != want {
		t.Errorf("the station was told %v about the supply, want order.update cancelled and order.cancelled, both %q", told, want)
	}

	// The Edge's row and the line's tile.
	if row := c.edgeRow(supply.ID); row.Status != protocol.StatusCancelled {
		t.Errorf("the Edge's supply row is %q, want cancelled", row.Status)
	}
	t.Logf("record: the Edge's supply row from its cancel: %v", r.edgeEnding(supply.ID))
	post := r.tile()
	postSeat, postCount := post.seat()
	t.Logf("after: tile %s", post)
	if len(post.Orders) != 0 || postSeat != *newBin || postCount != preCount ||
		!strings.Contains(post.AutoReorderNote, "Core cancelled order") {
		t.Errorf("the tile after: %s; want no order drawn, the seat still bin %d at %d (it was bin %d at %d), "+
			"and the position paused by Core's cancel", post, *newBin, preCount, preSeat, preCount)
	}

	// Whether anything names the half-swap.
	audits := r.audits(sp.ID)
	announced := r.announcedSince(m)
	t.Logf("record: supply audit %v; the Edge announced %+v", audits, announced)
	if slices.Contains(audits, "swap_half_completed") {
		t.Errorf("the supply's audit %v names a half-swap, want none: the cancel arm takes a delivered partner", audits)
	}
	if len(announced) != 1 || announced[0].OrderID != evac.ID {
		t.Errorf("the Edge announced %+v, want the evac's cancel alone: a partner whose sibling already ended is "+
			"not toasted", announced)
	}
}

// TestDeliveredOrder_WireCancel: the wire cancel (dispatch HandleOrderCancel)
// of a line delivery waiting on its receipt. The line is bare; REQUEST makes
// one plain delivery to it (bare_line_fill_test.go's door), minted with
// auto_confirm false (neither the claim nor the station sets it); the fleet
// carries it and sets the bin down. The operator then taps Abort on the Orders
// page (www apiCancelOrder: Manager.AbortOrderByPerson, the one call it
// makes), which the page draws for any order not ended, delivered included
// (orders-body.html).
//
// A plain order never reaches the parent's cascade (compound.go
// cancelCompoundChildren lists no children); the wire cancel of a dig parent
// does, and that is brief C's TestDeliveredLeg_ParentCancel, not repeated.
//
// Recorded today: the Edge ends its row and sends the cancel; Core cancels the
// delivered order (its only status screen is an order already cancelled),
// asks the fleet to stop the finished job and tells the station three times;
// the bin stays on the line, unclaimed, still the seat's bin with its count,
// and Core's sourcing predicate refuses it; the tile no longer lists the
// order, so the board draws no CONFIRM for it, and shows the pause the abort
// puts on the position.
func TestDeliveredOrder_WireCancel(t *testing.T) {
	t.Parallel()
	c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeTwoRobot, plain: true})
	r := newDloRig(c)
	c.takeLineBinOff()
	c.settle()

	before := c.lastID()
	c.operatorRequest()
	c.tick()
	var to []domain.Order
	for _, o := range c.newRows(before) {
		if o.DeliveryNode == ksrLine {
			to = append(to, o)
		}
	}
	if len(to) != 1 || to[0].OrderType == protocol.OrderTypeComplex || to[0].AutoConfirm {
		c.dump("the delivery")
		t.Fatalf("fixture: REQUEST on the bare line made %d orders to it (%+v), want one plain delivery needing a receipt",
			len(to), to)
	}
	d := to[0]
	c.driveUntil("the delivery landed", func() bool {
		c.fleetStep()
		return c.edgeRow(d.ID).Status == protocol.StatusDelivered && c.coreOf(d).Status == protocol.StatusDelivered
	})
	co := c.coreOf(d)
	cart := coreCart(co)
	if cart == nil || co.CompletedAt != nil {
		c.dump("the delivery landed")
		t.Fatalf("fixture: the delivery carries %v, completed=%v; want one cart, not completed", cart, co.CompletedAt != nil)
	}
	preBin, _ := r.cart(*cart)
	pre := r.tile()
	preSeat, preCount := pre.seat()
	t.Logf("before: %s %d delivered, bin %s (%d) at %s uop=%d; fleet %s; tile %s", d.OrderType, co.ID, preBin.Label,
		preBin.ID, preBin.NodeName, preBin.UOPRemaining, c.vendorState(co.VendorOrderID), pre)
	if !pre.lists(d.ID, protocol.StatusDelivered) || c.vendorState(co.VendorOrderID) != "FINISHED" || preSeat != *cart {
		t.Fatalf("fixture: the tile lists the delivery delivered %v, its job reads %q, the seat holds bin %d; want "+
			"delivered, FINISHED and bin %d bound by its landing", pre.lists(d.ID, protocol.StatusDelivered),
			c.vendorState(co.VendorOrderID), preSeat, *cart)
	}

	// The Orders page's Abort: the Edge ends its row and queues the cancel.
	m := r.mark()
	mustNil(t, c.edge.Engine.OrderManager().AbortOrderByPerson(d.ID), "the Orders page's Abort")
	if s := c.edgeRow(d.ID).Status; s != protocol.StatusCancelled {
		t.Errorf("after the Abort the Edge's row is %q, before Core has heard; want cancelled", s)
	}
	pending, err := c.edge.DB.ListPendingOutbox(100)
	mustNil(t, err, "the Edge's outbox")
	var sent []string
	for _, msg := range pending {
		var env protocol.Envelope
		mustNil(t, json.Unmarshal(msg.Payload, &env), "decode an Edge envelope")
		var body dloTell
		if json.Unmarshal(env.Payload, &body) == nil && body.OrderUUID == d.UUID {
			sent = append(sent, fmt.Sprintf("%s reason=%q", env.Type, body.Reason))
		}
	}
	t.Logf("record: the Edge queued %v", sent)
	if !slices.Equal(sent, []string{protocol.TypeOrderCancel + ` reason="aborted by operator"`}) {
		t.Errorf("the Edge queued %v about the order, want one order.cancel \"aborted by operator\"", sent)
	}
	c.settle()

	// The order's end at Core.
	co = c.coreOf(d)
	steps, code := r.ended(co.ID)
	t.Logf("record: %s %d %s (%q) completed=%v last=%v code=%q", co.OrderType, co.ID, co.Status, co.ErrorDetail,
		co.CompletedAt != nil, steps, code)
	if co.Status != protocol.StatusCancelled || co.ErrorDetail != "aborted by operator" || co.CompletedAt != nil {
		t.Errorf("Core's order is %q (%q) completed=%v, want cancelled \"aborted by operator\"", co.Status,
			co.ErrorDetail, co.CompletedAt != nil)
	}
	if !slices.Equal(steps, []protocol.Status{protocol.StatusDelivered, protocol.StatusCancelled}) ||
		code != string(protocol.TermOperatorCancelled) {
		t.Errorf("Core's history ends %v coded %q, want delivered then cancelled, coded %s", steps, code,
			protocol.TermOperatorCancelled)
	}

	// The cart: its row, its count, and whether Core would hand it on.
	b, sourceable := r.cart(*cart)
	pick := r.plantWidePick(ksrPartA)
	picked := "none"
	if pick != nil {
		picked = fmt.Sprintf("%s (%d) at %s", pick.Label, pick.ID, pick.NodeName)
	}
	t.Logf("record: bin %s (%d) at %s status=%s claimed_by=%v uop=%d (was %d) sourceable=%v; a plant-wide "+
		"ask for %s gets %s", b.Label, b.ID, b.NodeName, b.Status, b.ClaimedBy, b.UOPRemaining, preBin.UOPRemaining,
		sourceable, ksrPartA, picked)
	if b.NodeName != ksrLine || b.ClaimedBy != nil || b.UOPRemaining != preBin.UOPRemaining || sourceable ||
		(pick != nil && pick.ID == b.ID) {
		t.Errorf("the bin is at %s claimed by %v uop %d (was %d) sourceable=%v, picked %s; want it on the line, "+
			"unclaimed, its count unchanged and refused by the finders", b.NodeName, b.ClaimedBy, b.UOPRemaining,
			preBin.UOPRemaining, sourceable, picked)
	}

	// The fleet and the station.
	if st := c.vendorState(co.VendorOrderID); st != "STOPPED" {
		t.Errorf("the delivery's finished job reads %q at the fleet, want STOPPED: Core stops the job of the order it cancels", st)
	}
	told := r.toldSince(m, d.UUID)
	t.Logf("record: told the station: %v", told)
	var kinds []string
	for _, tl := range told {
		kinds = append(kinds, tl.Type+" "+tl.Status)
	}
	if !slices.Equal(kinds, []string{protocol.TypeOrderUpdate + " cancelled", protocol.TypeOrderCancelled + " ",
		protocol.TypeOrderCancelled + " "}) {
		t.Errorf("the station was told %v, want order.update cancelled, the cancel event's order.cancelled and the "+
			"dispatcher's reply", kinds)
	}

	// The Edge's row, the announcement and the line's tile.
	ending := r.edgeEnding(d.ID)
	t.Logf("record: the Edge's row from its cancel: %v", ending)
	if len(ending) != 1 || c.edgeRow(d.ID).Status != protocol.StatusCancelled {
		t.Errorf("the Edge's row from its cancel: %v, want the abort's one row and nothing after it", ending)
	}
	if a := r.announcedSince(m); len(a) != 0 {
		t.Errorf("the Edge announced %+v, want nothing: its own abort is not toasted", a)
	}
	post := r.tile()
	postSeat, postCount := post.seat()
	t.Logf("after: tile %s", post)
	if len(post.Orders) != 0 || postSeat != preSeat || postCount != preCount || preSeat != b.ID ||
		!strings.Contains(post.AutoReorderNote, "a person cancelled order") {
		t.Errorf("the tile after: %s; want no order drawn, the seat still bin %d at %d (it was bin %d at %d), and "+
			"the position paused by the abort", post, b.ID, preCount, preSeat, preCount)
	}
}
