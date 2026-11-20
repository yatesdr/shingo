// Two races between a terminal the operator writes at the Edge and a terminal
// Core writes on its own, pinned end to end as they behave today.
//
//  1. Abort against Core's own confirm. The operator aborts a delivered order:
//     the Edge writes cancelled and queues order.cancel. Before Core takes the
//     cancel, its stuck-delivered sweep confirms the order. The cancel then lands
//     on a terminal order and changes nothing, and Core's confirmed notice lands
//     on a cancelled Edge row and changes nothing.
//  2. Confirm against Core's own cancel. Core cancels a delivered order; before
//     its order.cancelled reaches the Edge, the operator confirms there: the Edge
//     writes confirmed and queues its receipt. Core refuses the receipt on a
//     cancelled order, and the order.cancelled lands on a confirmed Edge row and
//     changes nothing.
//
// Both end with the two sides disagreeing about how the order ended. These are
// known pairs and they stay as they are until abort and confirm become intents
// the Edge asks Core for rather than terminals it writes itself. Until then a
// change to how Core tells the Edge an order's status must leave both pairs
// where these tests find them.
//
// Each race is made deterministic by holding one direction of the Bus at a
// time: a held side's outbox reads empty to the pump, so nothing it queued is
// delivered until the test lets it go. Each pair runs in both delivery orders,
// Core's message first and the Edge's first; the end is the same in both.
//
//go:build docker

package scenarios

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/router"

	"shingocore/dispatch"
	coremessaging "shingocore/messaging"
	corestore "shingocore/store"
	corestoremessaging "shingocore/store/messaging"
	coreorders "shingocore/store/orders"
	coreharness "shingocore/testharness"

	"shingoedge/domain"
	edgeorders "shingoedge/orders"
	edgemessaging "shingoedge/store/messaging"
	edgeharness "shingoedge/testharness"
)

const (
	sppStation = "edge.spp"
	sppRobot   = "SPP-AMR"
	// sppCoreCancel is the reason Core's own cancel of the delivered order gives.
	sppCoreCancel = "cancelled at Core while the station confirms"
)

// sppMsg is one order message as it crossed the Bus: its type and the payload
// fields the pins read.
type sppMsg struct {
	Type        string
	OrderUUID   string `json:"order_uuid"`
	Status      string `json:"status"`
	Detail      string `json:"detail"`
	Reason      string `json:"reason"`
	ReceiptType string `json:"receipt_type"`
	FinalCount  int64  `json:"final_count"`
}

func sppDecode(t *testing.T, raw []byte) sppMsg {
	t.Helper()
	var env protocol.Envelope
	mustNil(t, json.Unmarshal(raw, &env), "decode envelope")
	var m sppMsg
	if len(env.Payload) > 0 {
		_ = json.Unmarshal(env.Payload, &m) // a data envelope has no order fields
	}
	m.Type = env.Type
	return m
}

// sppHold is one direction of the Bus: while held, its outbox reads empty to
// the pump; every message the pump is handed is recorded as sent.
type sppHold struct {
	mu   sync.Mutex
	held bool
	sent []sppMsg
}

func (h *sppHold) hold(on bool) {
	h.mu.Lock()
	h.held = on
	h.mu.Unlock()
}

// forOrder is what this side has sent about one order since the last reset.
func (h *sppHold) forOrder(uuid string) []sppMsg {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []sppMsg
	for _, m := range h.sent {
		if m.OrderUUID == uuid {
			out = append(out, m)
		}
	}
	return out
}

func (h *sppHold) reset() {
	h.mu.Lock()
	h.sent = nil
	h.mu.Unlock()
}

type sppEdgeOutbox struct {
	harness.EdgeOutboxOps
	t *testing.T
	sppHold
}

func (o *sppEdgeOutbox) ListPendingOutbox(limit int) ([]edgemessaging.Message, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.held {
		return nil, nil
	}
	msgs, err := o.EdgeOutboxOps.ListPendingOutbox(limit)
	for _, m := range msgs {
		o.sent = append(o.sent, sppDecode(o.t, m.Payload))
	}
	return msgs, err
}

// pending is what the Edge has queued about one order and not yet sent.
func (o *sppEdgeOutbox) pending(uuid string) []sppMsg {
	msgs, err := o.EdgeOutboxOps.ListPendingOutbox(1000)
	mustNil(o.t, err, "edge outbox")
	var out []sppMsg
	for _, m := range msgs {
		if d := sppDecode(o.t, m.Payload); d.OrderUUID == uuid {
			out = append(out, d)
		}
	}
	return out
}

type sppCoreOutbox struct {
	harness.CoreOutboxOps
	t *testing.T
	sppHold
}

func (o *sppCoreOutbox) ListPendingOutbox(limit int) ([]*corestoremessaging.OutboxMessage, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.held {
		return nil, nil
	}
	msgs, err := o.CoreOutboxOps.ListPendingOutbox(limit)
	for _, m := range msgs {
		o.sent = append(o.sent, sppDecode(o.t, m.Payload))
	}
	return msgs, err
}

// pending is what Core has queued about one order and not yet sent.
func (o *sppCoreOutbox) pending(uuid string) []sppMsg {
	msgs, err := o.CoreOutboxOps.ListPendingOutbox(1000)
	mustNil(o.t, err, "core outbox")
	var out []sppMsg
	for _, m := range msgs {
		if d := sppDecode(o.t, m.Payload); d.OrderUUID == uuid {
			out = append(out, d)
		}
	}
	return out
}

// sppCell is a real Core engine on the fleet simulator and a real Edge engine,
// wired through the Bus, with one full bin in storage for the Edge to call to
// its line.
type sppCell struct {
	t       *testing.T
	core    kscCore
	coreDB  *corestore.DB
	edge    *edgeharness.Edge
	bus     *harness.Bus
	edgeOut *sppEdgeOutbox
	coreOut *sppCoreOutbox
	std     *coreharness.StandardData
}

func newSppCell(t *testing.T) *sppCell {
	t.Helper()
	core := startKeepStagedCore(t)
	coreDB := core.eng.DB()
	std := coreharness.SetupStandardData(t, coreDB)
	mustNil(t, coreDB.SetPayloadBinTypes(std.Payload.ID, []int64{std.BinType.ID}), "carrier rule")
	coreharness.CreateBinAtNode(t, coreDB, std.Payload.Code, std.StorageNode.ID, "SPP-BIN")

	edge := edgeharness.NewEdge(t, sppStation)

	coreHandler := coremessaging.NewCoreHandler(coreDB, nil, "core", "shingo.dispatch", core.eng.Dispatcher())
	coreIngestor := protocol.NewIngestor(nil)
	coreRouter := router.New[string]()
	router.Register(coreRouter, protocol.TypeOrderRequest, coreHandler.HandleOrderRequest)
	router.Register(coreRouter, protocol.TypeOrderCancel, coreHandler.HandleOrderCancel)
	router.Register(coreRouter, protocol.TypeOrderReceipt, coreHandler.HandleOrderReceipt)
	router.Register(coreRouter, protocol.TypeOrderRelease, coreHandler.HandleOrderRelease)
	coreIngestor.Dispatch = func(env *protocol.Envelope) { coreRouter.Dispatch(env, env.Type) }

	c := &sppCell{t: t, core: core, coreDB: coreDB, edge: edge, std: std}
	c.edgeOut = &sppEdgeOutbox{EdgeOutboxOps: edge.DB, t: t}
	c.coreOut = &sppCoreOutbox{CoreOutboxOps: coreDB, t: t}
	c.bus = harness.NewBus(t,
		harness.EdgeSide{EdgeStore: c.edgeOut, EdgeIngestor: edge.Ingestor},
		harness.CoreSide{CoreStore: c.coreOut, CoreIngestor: coreIngestor},
	)
	c.settle()
	return c
}

// settle pumps whatever is not held until both sides are quiet.
func (c *sppCell) settle() {
	for i := 0; i < 20; i++ {
		if c.bus.PumpAll() == 0 {
			time.Sleep(50 * time.Millisecond)
			if c.bus.PumpAll() == 0 {
				return
			}
		}
	}
}

func (c *sppCell) eventually(what string, ok func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c.settle()
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.t.Fatalf("timed out waiting: %s", what)
}

func (c *sppCell) edgeRow(id int64) *domain.Order {
	c.t.Helper()
	o, err := c.edge.DB.GetOrder(id)
	mustNil(c.t, err, "edge order")
	return o
}

func (c *sppCell) coreRow(uuid string) *coreorders.Order {
	c.t.Helper()
	o, err := c.coreDB.GetOrderByUUID(uuid)
	mustNil(c.t, err, "core order "+uuid)
	return o
}

// delivered calls the bin to the line from the Edge and drives the robot to the
// end: the order reads delivered on both sides, with nothing in flight and
// nothing confirmed. The record of what each side sent starts after it.
func (c *sppCell) delivered() domain.Order {
	c.t.Helper()
	o, err := c.edge.Engine.OrderManager().CreateRetrieveOrder(edgeorders.RetrieveSpec{
		Delivery: c.std.LineNode.Name, Source: c.std.StorageNode.Name, Payload: c.std.Payload.Code,
		Quantity: 1, Origin: edgeorders.NoDemand(),
	})
	mustNil(c.t, err, "the call")
	c.eventually("the call with the fleet", func() bool {
		co, err := c.coreDB.GetOrderByUUID(o.UUID)
		return err == nil && co.VendorOrderID != ""
	})
	vendor := c.coreRow(o.UUID).VendorOrderID
	c.core.sim.DriveStateWithRobot(vendor, "RUNNING", sppRobot)
	c.settle()
	c.core.sim.DriveStateWithRobot(vendor, "FINISHED", sppRobot)
	c.eventually("delivered on both sides", func() bool {
		return c.coreRow(o.UUID).Status == protocol.StatusDelivered && c.edgeRow(o.ID).Status == protocol.StatusDelivered
	})
	if co := c.coreRow(o.UUID); co.CompletedAt != nil {
		c.t.Fatalf("Core order %d is completed at delivered, want it waiting for the station", co.ID)
	}
	if p := c.edgeOut.pending(o.UUID); len(p) != 0 {
		c.t.Fatalf("the Edge still holds %v for the order at delivered, want nothing", p)
	}
	if p := c.coreOut.pending(o.UUID); len(p) != 0 {
		c.t.Fatalf("Core still holds %v for the order at delivered, want nothing", p)
	}
	c.edgeOut.reset()
	c.coreOut.reset()
	return *c.edgeRow(o.ID)
}

// coreSweep runs Core's stuck-delivered sweep (the reconciliation loop's
// auto-confirm, ReconciliationService.AutoConfirmStuckDeliveredOrders) with a
// timeout the order has already outlived, until it confirms the one order.
func (c *sppCell) coreSweep() {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, err := c.core.eng.Reconciliation().AutoConfirmStuckDeliveredOrders(time.Millisecond)
		mustNil(c.t, err, "stuck-delivered sweep")
		if n == 1 {
			return
		}
		if n > 1 {
			c.t.Fatalf("the sweep confirmed %d orders, want the one delivered order", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatal("the stuck-delivered sweep never confirmed the delivered order")
}

func (c *sppCell) coreHistory(id int64) []protocol.Status {
	c.t.Helper()
	rows, err := c.coreDB.ListOrderHistory(id)
	mustNil(c.t, err, "core history")
	var out []protocol.Status
	for _, h := range rows {
		out = append(out, h.Status)
	}
	return out
}

func (c *sppCell) edgeHistory(id int64) []protocol.Status {
	c.t.Helper()
	rows, err := c.edge.DB.ListOrderHistory(id)
	mustNil(c.t, err, "edge history")
	var out []protocol.Status
	for _, h := range rows {
		out = append(out, h.NewStatus)
	}
	return out
}

func sppHas(list []protocol.Status, s protocol.Status) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// sppDeliveryOrders are the two orders the held messages can cross in.
var sppDeliveryOrders = []struct {
	name      string
	coreFirst bool
}{
	{"Core's message first", true},
	{"the Edge's message first", false},
}

// TestStatusPairPin_AbortVsCoreAutoConfirm pins the abort that loses to Core's
// own confirm. The operator aborts a delivered order at the Edge (Manager.
// AbortOrder: the Edge writes cancelled and queues order.cancel). Before the
// cancel reaches Core, Core's stuck-delivered sweep confirms the order through
// lifecycle ConfirmReceipt and tells the Edge with order.update confirmed. The
// cancel lands on a terminal order: CancelOrder returns without a write, and
// the dispatcher still answers order.cancelled. Neither message moves the
// Edge's cancelled row: ApplyCoreStatus's confirmed arm returns on a terminal
// row, and a cancelled reply on a cancelled row is idempotent.
//
// End: Core confirmed, the Edge cancelled. A known pair, kept as it is until
// abort and confirm become intents.
func TestStatusPairPin_AbortVsCoreAutoConfirm(t *testing.T) {
	t.Parallel()
	for _, order := range sppDeliveryOrders {
		t.Run(order.name, func(t *testing.T) {
			t.Parallel()
			c := newSppCell(t)
			o := c.delivered()

			// The operator aborts. The Edge writes cancelled and holds the cancel.
			c.edgeOut.hold(true)
			mustNil(t, c.edge.Engine.OrderManager().AbortOrder(o.ID), "abort")
			if s := c.edgeRow(o.ID).Status; s != protocol.StatusCancelled {
				t.Fatalf("after the abort the Edge reads %q, want cancelled", s)
			}
			if p := c.edgeOut.pending(o.UUID); len(p) != 1 || p[0].Type != protocol.TypeOrderCancel {
				t.Fatalf("after the abort the Edge holds %v, want one order.cancel", p)
			}

			// Core, not yet told, confirms the order on its own.
			c.coreOut.hold(true)
			c.coreSweep()
			co := c.coreRow(o.UUID)
			if co.Status != protocol.StatusConfirmed || co.CompletedAt == nil {
				t.Fatalf("after the sweep Core reads %q (completed %v), want confirmed and completed",
					co.Status, co.CompletedAt != nil)
			}
			if p := c.coreOut.pending(o.UUID); len(p) != 1 || p[0].Type != protocol.TypeOrderUpdate ||
				p[0].Status != string(protocol.StatusConfirmed) {
				t.Fatalf("after the sweep Core holds %v, want one order.update confirmed", p)
			}

			if order.coreFirst {
				// Core's confirmed notice reaches the Edge's cancelled row.
				c.coreOut.hold(false)
				c.settle()
				if s := c.edgeRow(o.ID).Status; s != protocol.StatusCancelled {
					t.Fatalf("Core's confirmed notice moved the Edge's cancelled row to %q", s)
				}
				// Then the Edge's cancel reaches Core's confirmed order.
				c.edgeOut.hold(false)
				c.settle()
			} else {
				// The Edge's cancel reaches Core's confirmed order.
				c.edgeOut.hold(false)
				c.settle()
				if s := c.coreRow(o.UUID).Status; s != protocol.StatusConfirmed {
					t.Fatalf("the Edge's cancel moved Core's confirmed order to %q", s)
				}
				// Then Core's notice and its reply to the cancel reach the Edge.
				c.coreOut.hold(false)
				c.settle()
			}

			// The end: the two sides disagree, and stay that way.
			co = c.coreRow(o.UUID)
			if co.Status != protocol.StatusConfirmed || co.CompletedAt == nil {
				t.Errorf("end: Core reads %q (completed %v), want confirmed and completed", co.Status, co.CompletedAt != nil)
			}
			if h := c.coreHistory(co.ID); sppHas(h, protocol.StatusCancelled) {
				t.Errorf("end: Core's history %v has a cancelled row, want none: the cancel lands on a terminal order", h)
			}
			if s := c.edgeRow(o.ID).Status; s != protocol.StatusCancelled {
				t.Errorf("end: the Edge reads %q, want cancelled", s)
			}
			if h := c.edgeHistory(o.ID); sppHas(h, protocol.StatusConfirmed) {
				t.Errorf("end: the Edge's history %v has a confirmed row, want none", h)
			}

			// What crossed the Bus about the order after it was delivered.
			edgeSent := c.edgeOut.forOrder(o.UUID)
			if len(edgeSent) != 1 || edgeSent[0].Type != protocol.TypeOrderCancel || edgeSent[0].Reason != "aborted by operator" {
				t.Errorf("the Edge sent %+v, want one order.cancel \"aborted by operator\"", edgeSent)
			}
			coreSent := c.coreOut.forOrder(o.UUID)
			if len(coreSent) != 2 ||
				coreSent[0].Type != protocol.TypeOrderUpdate || coreSent[0].Status != string(protocol.StatusConfirmed) ||
				coreSent[1].Type != protocol.TypeOrderCancelled || coreSent[1].Reason != "aborted by operator" {
				t.Errorf("Core sent %+v, want order.update confirmed, then order.cancelled \"aborted by operator\" "+
					"(the dispatcher's answer to the cancel, sent though its order reads confirmed)", coreSent)
			}
			if p := c.edgeOut.pending(o.UUID); len(p) != 0 {
				t.Errorf("end: the Edge still holds %v", p)
			}
			if p := c.coreOut.pending(o.UUID); len(p) != 0 {
				t.Errorf("end: Core still holds %v", p)
			}
		})
	}
}

// TestStatusPairPin_ConfirmVsCoreCancel pins the confirm that loses to Core's
// own cancel. Core cancels a delivered order through the lifecycle cancel
// (LifecycleService.CancelOrder, the door the swap-peer cascade and the fleet
// stop take; the admin terminate refuses a delivered order), and tells the Edge
// twice: the cancel's status write queues its order.update cancelled, and the
// cancellation subscriber its order.cancelled. Before either reaches the Edge, the operator confirms
// there (Manager.ConfirmDelivery: the Edge writes confirmed and queues
// order.receipt). Core's ConfirmReceipt refuses cancelled to confirmed and
// answers nothing. Core's cancelled, told and announced, lands on the Edge's
// confirmed row and does not move it.
//
// End: Core cancelled, the Edge confirmed. A known pair, kept as it is until
// abort and confirm become intents.
func TestStatusPairPin_ConfirmVsCoreCancel(t *testing.T) {
	t.Parallel()
	for _, order := range sppDeliveryOrders {
		t.Run(order.name, func(t *testing.T) {
			t.Parallel()
			c := newSppCell(t)
			o := c.delivered()

			// Core cancels the delivered order and holds its notice.
			c.coreOut.hold(true)
			co := c.coreRow(o.UUID)
			c.core.eng.Dispatcher().Lifecycle().CancelOrder(co, co.StationUID, sppCoreCancel, dispatch.CancelCause{})
			if s := c.coreRow(o.UUID).Status; s != protocol.StatusCancelled {
				t.Fatalf("after Core's cancel Core reads %q, want cancelled", s)
			}
			if p := c.coreOut.pending(o.UUID); len(p) != 2 || p[0].Type != protocol.TypeOrderUpdate ||
				p[0].Status != string(protocol.StatusCancelled) || p[1].Type != protocol.TypeOrderCancelled {
				t.Fatalf("after Core's cancel Core holds %v, want its order.update cancelled, then order.cancelled", p)
			}

			// The operator, not yet told, confirms at the Edge.
			c.edgeOut.hold(true)
			mustNil(t, c.edge.Engine.OrderManager().ConfirmDelivery(o.ID, 1), "confirm")
			if s := c.edgeRow(o.ID).Status; s != protocol.StatusConfirmed {
				t.Fatalf("after the confirm the Edge reads %q, want confirmed", s)
			}
			if p := c.edgeOut.pending(o.UUID); len(p) != 1 || p[0].Type != protocol.TypeOrderReceipt {
				t.Fatalf("after the confirm the Edge holds %v, want one order.receipt", p)
			}

			if order.coreFirst {
				// Core's cancelled reaches the Edge's confirmed row.
				c.coreOut.hold(false)
				c.settle()
				if s := c.edgeRow(o.ID).Status; s != protocol.StatusConfirmed {
					t.Fatalf("Core's cancelled moved the Edge's confirmed row to %q", s)
				}
				// Then the Edge's receipt reaches Core's cancelled order.
				c.edgeOut.hold(false)
				c.settle()
			} else {
				// The Edge's receipt reaches Core's cancelled order.
				c.edgeOut.hold(false)
				c.settle()
				if s := c.coreRow(o.UUID).Status; s != protocol.StatusCancelled {
					t.Fatalf("the Edge's receipt moved Core's cancelled order to %q", s)
				}
				// Then Core's cancelled reaches the Edge's confirmed row.
				c.coreOut.hold(false)
				c.settle()
			}

			// The end: the two sides disagree, and stay that way.
			co = c.coreRow(o.UUID)
			if co.Status != protocol.StatusCancelled || co.CompletedAt != nil {
				t.Errorf("end: Core reads %q (completed %v), want cancelled and not completed", co.Status, co.CompletedAt != nil)
			}
			if h := c.coreHistory(co.ID); sppHas(h, protocol.StatusConfirmed) {
				t.Errorf("end: Core's history %v has a confirmed row, want none: the receipt is refused", h)
			}
			if s := c.edgeRow(o.ID).Status; s != protocol.StatusConfirmed {
				t.Errorf("end: the Edge reads %q, want confirmed", s)
			}
			if h := c.edgeHistory(o.ID); sppHas(h, protocol.StatusCancelled) {
				t.Errorf("end: the Edge's history %v has a cancelled row, want none", h)
			}

			// What crossed the Bus about the order after it was delivered.
			edgeSent := c.edgeOut.forOrder(o.UUID)
			if len(edgeSent) != 1 || edgeSent[0].Type != protocol.TypeOrderReceipt ||
				edgeSent[0].ReceiptType != "confirmed" || edgeSent[0].FinalCount != 1 {
				t.Errorf("the Edge sent %+v, want one order.receipt confirmed, count 1", edgeSent)
			}
			coreSent := c.coreOut.forOrder(o.UUID)
			if len(coreSent) != 2 || coreSent[0].Type != protocol.TypeOrderUpdate || coreSent[0].Status != string(protocol.StatusCancelled) ||
				coreSent[0].Detail != sppCoreCancel || coreSent[1].Type != protocol.TypeOrderCancelled || coreSent[1].Reason != sppCoreCancel {
				t.Errorf("Core sent %+v, want its order.update cancelled and order.cancelled, both %q, and no answer to the refused receipt",
					coreSent, sppCoreCancel)
			}
			if p := c.edgeOut.pending(o.UUID); len(p) != 0 {
				t.Errorf("end: the Edge still holds %v", p)
			}
			if p := c.coreOut.pending(o.UUID); len(p) != 0 {
				t.Errorf("end: Core still holds %v", p)
			}
		})
	}
}
