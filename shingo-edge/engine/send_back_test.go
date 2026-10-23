package engine

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	ordermgr "shingoedge/orders"
	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// send_back_test.go — SEND BACK on a tote press (operator_send_back.go): the
// route's two orders on the outside's own process node, what refuses it, its
// CANCEL, a REQUEST during it, and the board's line.

const (
	sbPress, sbOut, sbSuper, sbUnload, sbPart = "SB-PRESS", "SB-OUT", "SB-SUPER", "SB-UNLOAD", "SB-PART"
)

type sbCell struct {
	eng                         *Engine
	db                          *store.DB
	processID, press, outside   int64
	stationID, styleID, claimID int64
}

func sbInt(n int) *int { return &n }

// sbCart is a tote cart's row in the bins copy: 8 a layer, totes on it.
func sbCart(node, label string, id int64, totes int) protocol.NodeBinsRow {
	return protocol.NodeBinsRow{Node: node, Occupied: true, BinID: id, BinLabel: label, BinTypeCode: "SB-TOTE",
		TotesPerLayer: sbInt(8), TotesOnCart: sbInt(totes)}
}

// seedSendBackPress is a produce tote press fed from behind, its outside a
// process node of the same process with no station, a cart 3 high outside and
// tops & bottoms on the press, in the bins copy.
func seedSendBackPress(t *testing.T, db *store.DB) sbCell {
	t.Helper()
	c := sbCell{db: db}
	var err error
	c.processID, err = db.CreateProcess("SB-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	c.stationID, err = db.CreateOperatorStation(stations.Input{ProcessID: c.processID, Code: "SB-ST", Name: "SB Station",
		Sequence: 1, Enabled: true})
	testutil.MustNoErr(t, err, "station")
	c.press, err = db.CreateProcessNode(processes.NodeInput{ProcessID: c.processID, OperatorStationID: &c.stationID,
		CoreNodeName: sbPress, Code: "SB1", Name: sbPress, Sequence: 1, Enabled: true})
	testutil.MustNoErr(t, err, "press")
	c.outside, err = db.CreateProcessNode(processes.NodeInput{ProcessID: c.processID, CoreNodeName: sbOut,
		Code: "SB2", Name: sbOut, Sequence: 2, Enabled: true})
	testutil.MustNoErr(t, err, "outside")
	c.styleID, err = db.CreateStyle("SB-STYLE", "", c.processID)
	testutil.MustNoErr(t, err, "style")
	testutil.MustNoErr(t, db.SetActiveStyle(c.processID, &c.styleID), "active style")
	c.claimID, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: c.styleID, CoreNodeName: sbPress, Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeTwoRobotPressIndex, PayloadCode: sbPart, UOPCapacity: 40,
		InboundSource: sbSuper, OutboundDestination: sbUnload, PairedCoreNode: sbOut,
	})
	testutil.MustNoErr(t, err, "claim")
	_, err = db.EnsureProcessNodeRuntime(c.press)
	testutil.MustNoErr(t, err, "runtime")
	c.eng = testEngine(t, db)
	c.hold(t, sbCart(sbOut, "SB-CART", 77, 24), sbCart(sbPress, "SB-FRONT", 76, 0))
	return c
}

func (c sbCell) hold(t *testing.T, rows ...protocol.NodeBinsRow) {
	t.Helper()
	holdActableBins(t, c.eng, rows...)
}

func (c sbCell) row(t *testing.T, id int64) *domain.Order {
	t.Helper()
	o, err := c.db.GetOrder(id)
	testutil.MustNoErr(t, err, "order")
	return o
}

// TestSendBack_TheMoveAndThePullOnTheOutside: the cart outside goes by name to
// the outbound, then tops & bottoms of the press's part comes from its inbound
// source, both on the outside's process node, auto-confirmed and owed to no
// demand, in that order; the press's runtime slots are untouched.
func TestSendBack_TheMoveAndThePullOnTheOutside(t *testing.T) {
	t.Parallel()
	c := seedSendBackPress(t, testEngineDB(t))
	res, err := c.eng.SendBack(c.press)
	testutil.MustNoErr(t, err, "SEND BACK")
	m, p := res.Move, res.Pull
	if m.OrderType != protocol.OrderTypeMove || m.SourceNode != sbOut || m.DeliveryNode != sbUnload ||
		m.ProcessNodeID == nil || *m.ProcessNodeID != c.outside || !m.AutoConfirm {
		t.Errorf("the move: %+v", m)
	}
	if p.OrderType != protocol.OrderTypeRetrieveTopsAndBottoms || p.DeliveryNode != sbOut || p.SourceNode != sbSuper ||
		p.PayloadCode != sbPart || p.ProcessNodeID == nil || *p.ProcessNodeID != c.outside || !p.AutoConfirm || p.Quantity != 1 {
		t.Errorf("the pull: %+v", p)
	}
	rows, err := c.db.ListActiveOrdersByProcessNode(c.outside)
	testutil.MustNoErr(t, err, "the outside's orders")
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	if len(rows) != 2 || rows[0].ID != m.ID || rows[1].ID != p.ID {
		t.Errorf("the outside's orders: %+v, want the move then the pull and nothing else", rows)
	}
	for _, o := range []*domain.Order{m, p} {
		if o.OriginClass != string(protocol.OriginClassNoDemand) {
			t.Errorf("order %d owes %q, want no demand", o.ID, o.OriginClass)
		}
	}
	msgs, err := c.db.ListPendingOutbox(100)
	testutil.MustNoErr(t, err, "outbox")
	named := false
	for _, msg := range msgs {
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(msg.Payload, &env), "envelope")
		var req protocol.OrderRequest
		if msg.MsgType == protocol.TypeOrderRequest && env.DecodePayload(&req) == nil && req.OrderUUID == m.UUID {
			named = req.BinID == 77
		}
	}
	if !named {
		t.Error("the move does not name the cart (bin 77) on the wire")
	}
	rt, err := c.db.GetProcessNodeRuntime(c.press)
	testutil.MustNoErr(t, err, "runtime")
	if rt.ActiveOrderID != nil || rt.StagedOrderID != nil {
		t.Errorf("the press's runtime slots were written: %+v", rt)
	}
}

// TestSendBack_WhatRefusesIt: a second tap meets SEND BACK in flight; a swap
// queued on the press disables it; tops & bottoms already outside leaves
// nothing to send back; an outside with no process node row says so.
func TestSendBack_WhatRefusesIt(t *testing.T) {
	t.Parallel()
	t.Run("double tap", func(t *testing.T) {
		t.Parallel()
		c := seedSendBackPress(t, testEngineDB(t))
		_, err := c.eng.SendBack(c.press)
		testutil.MustNoErr(t, err, "first tap")
		if _, err := c.eng.SendBack(c.press); err == nil || !strings.Contains(err.Error(), "already in progress") {
			t.Fatalf("second tap: %v, want refused as in progress", err)
		}
		live, err := c.db.ListActiveOrdersByProcessNode(c.outside)
		testutil.MustNoErr(t, err, "orders")
		if len(live) != 2 {
			t.Errorf("%d orders on the outside after a double tap, want one pair", len(live))
		}
	})
	t.Run("a swap queued on the press", func(t *testing.T) {
		t.Parallel()
		c := seedSendBackPress(t, testEngineDB(t))
		leg, err := c.eng.orderMgr.CreateComplexOrder(&c.press, 1, sbUnload, sbPress,
			[]protocol.ComplexOrderStep{{Action: protocol.ActionPickup, Node: sbPress}, {Action: protocol.ActionDropoff, Node: sbUnload}},
			ordermgr.NoDemand())
		testutil.MustNoErr(t, err, "a swap leg")
		testutil.MustNoErr(t, c.db.UpdateProcessNodeRuntimeOrders(c.press, &leg.ID, nil), "the press's slot")
		if _, err := c.eng.SendBack(c.press); err == nil || !strings.Contains(err.Error(), "swap is queued or in flight") {
			t.Fatalf("SEND BACK with a swap queued: %v", err)
		}
	})
	t.Run("tops & bottoms already outside", func(t *testing.T) {
		t.Parallel()
		c := seedSendBackPress(t, testEngineDB(t))
		c.hold(t, sbCart(sbOut, "SB-TB", 78, 0), sbCart(sbPress, "SB-FRONT", 76, 0))
		if _, err := c.eng.SendBack(c.press); err == nil || !strings.Contains(err.Error(), "nothing to send back") {
			t.Fatalf("SEND BACK with tops & bottoms outside: %v", err)
		}
	})
	t.Run("an outside with no process node", func(t *testing.T) {
		t.Parallel()
		c := seedSendBackPress(t, testEngineDB(t))
		testutil.MustNoErr(t, c.db.DeleteProcessNode(c.outside), "drop the outside's row")
		if _, err := c.eng.SendBack(c.press); err == nil || err.Error() != sbOut+" has no process node on SB-PROC" {
			t.Fatalf("SEND BACK with no outside row: %v", err)
		}
	})
}

// TestCancelSendBack_AbortsOnlyTheUnmoved: the move has lifted; CANCEL aborts
// the pull and leaves the move to finish.
func TestCancelSendBack_AbortsOnlyTheUnmoved(t *testing.T) {
	t.Parallel()
	c := seedSendBackPress(t, testEngineDB(t))
	res, err := c.eng.SendBack(c.press)
	testutil.MustNoErr(t, err, "SEND BACK")
	testutil.MustNoErr(t, c.db.UpdateOrderStatus(res.Move.ID, string(protocol.StatusInTransit)), "the move lifts")
	n, err := c.eng.CancelSendBack(c.press)
	testutil.MustNoErr(t, err, "CANCEL")
	if n != 1 {
		t.Errorf("cancelled %d, want the pull alone", n)
	}
	if s := c.row(t, res.Move.ID).Status; s != protocol.StatusInTransit {
		t.Errorf("the moving cart's order is %s, want it left to finish", s)
	}
	if s := c.row(t, res.Pull.ID).Status; s != protocol.StatusCancelled {
		t.Errorf("the pull is %s, want cancelled", s)
	}
}

// TestSendBack_ARequestDuringItQueues: REQUEST SWAP on a press whose SEND
// BACK is live builds its swap; it queues at Core behind the carrier coming.
func TestSendBack_ARequestDuringItQueues(t *testing.T) {
	t.Parallel()
	c := seedSendBackPress(t, testEngineDB(t))
	_, err := c.eng.SendBack(c.press)
	testutil.MustNoErr(t, err, "SEND BACK")
	testutil.MustNoErr(t, c.db.SetProcessNodeRuntime(c.press, &c.claimID, 30), "a count")
	res, err := c.eng.RequestProduceSwap(c.press, 0)
	testutil.MustNoErr(t, err, "REQUEST SWAP during SEND BACK")
	if res.OrderA == nil || res.OrderB == nil {
		t.Errorf("REQUEST SWAP during SEND BACK made %+v, want the swap", res)
	}
}

// TestStationView_SendBackLine: the board's line on the press, from the
// board's own rows: enabled with the cart and the outbound named, then
// waiting with CANCEL once SEND BACK has gone, and the steady view issues the
// same statements as a board whose press is no SEND BACK press (consume): the
// outside rides the one order read.
//
// NOT PARALLEL: the held node rows are current only while
// processes.NodeGeneration stands still, and that counter is the process's, so
// a parallel test writing process nodes would move it between two polls here.
func TestStationView_SendBackLine(t *testing.T) {
	build := func(t *testing.T, c sbCell) *domain.StationNodeView {
		t.Helper()
		view, err := c.eng.stationService.BuildView(context.Background(), c.stationID)
		testutil.MustNoErr(t, err, "BuildView")
		c.eng.stationService.StampBins(view, service.BoardBinsFromCopy(c.eng.LocalNodeBins().Rows), true, nil)
		for i := range view.Nodes {
			if view.Nodes[i].Node.ID == c.press {
				return &view.Nodes[i]
			}
		}
		t.Fatal("no press tile")
		return nil
	}
	db, counter := testEngineDBCounting(t)
	c := seedSendBackPress(t, db)
	// The first poll after process_nodes moves draws no line: the held node
	// rows are refreshed by that poll's stamp, and read from the next.
	if tile := build(t, c); tile.SendBack != nil {
		t.Fatalf("the first poll drew a line from rows not yet held: %+v", tile.SendBack)
	}
	c.hold(t, sbCart(sbOut, "SB-CART", 77, 24), sbCart(sbPress, "SB-FRONT", 76, 0))
	tile := build(t, c)
	if tile.SendBack == nil || tile.SendBack.State != domain.SendBackEnabled || tile.SendBack.Bin != "SB-CART" ||
		tile.SendBack.Outbound != sbUnload {
		t.Fatalf("the line: %+v", tile.SendBack)
	}
	counter.Reset()
	build(t, c)
	fed := len(counter.Statements())

	_, err := c.eng.SendBack(c.press)
	testutil.MustNoErr(t, err, "SEND BACK")
	if tile = build(t, c); tile.SendBack == nil || tile.SendBack.State != domain.SendBackWaiting || !tile.SendBack.CanCancel {
		t.Fatalf("the line after SEND BACK: %+v", tile.SendBack)
	}

	db2, counter2 := testEngineDBCounting(t)
	plain := seedSendBackPress(t, db2)
	_, err = db2.DB.Exec(`UPDATE style_node_claims SET role = 'consume' WHERE id = ?`, plain.claimID)
	testutil.MustNoErr(t, err, "the same press, consume")
	build(t, plain)
	plain.hold(t, sbCart(sbOut, "SB-CART", 77, 24), sbCart(sbPress, "SB-FRONT", 76, 0))
	if tile := build(t, plain); tile.SendBack != nil {
		t.Fatalf("a consume press has a line: %+v", tile.SendBack)
	}
	counter2.Reset()
	build(t, plain)
	if n := len(counter2.Statements()); fed != n {
		t.Errorf("the steady view issues %d statements with SEND BACK's line, %d without: want the same", fed, n)
	}
}
