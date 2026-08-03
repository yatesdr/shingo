//go:build docker

package messaging

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// capturingResponder records the reply instead of enqueueing it, so a test can
// read what Core actually offered the Edge.
type capturingResponder struct {
	subject string
	payload any
}

func (c *capturingResponder) replyData(_ *protocol.Envelope, subject string, payload any) {
	c.subject = subject
	c.payload = payload
}

func (c *capturingResponder) sendData(subject, _ string, payload any) {
	c.subject = subject
	c.payload = payload
}

func (c *capturingResponder) dbg(string, ...any) {}

func edgeAsking(station string, uuids ...string) (*protocol.Envelope, *protocol.OrderStatusRequest) {
	return &protocol.Envelope{
			ID:  "msg-reconcile",
			Src: protocol.Address{Role: protocol.RoleEdge, Station: station},
			Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"},
		},
		&protocol.OrderStatusRequest{OrderUUIDs: uuids}
}

// TestOrderReconcile_OffersOrdersTheEdgeDidNotAskAbout is the healing half of
// the reconcile, and the reason the reconcile is load-bearing rather than a
// backstop: the Core → Edge outbox drops a message permanently once it is past
// its retries, so an order Core authored may simply never have reached the Edge.
// Nothing else repairs that.
func TestOrderReconcile_OffersOrdersTheEdgeDidNotAskAbout(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := &nodes.Node{Name: "RC-W1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create node")

	known := &orders.Order{
		EdgeUUID: "rc-known", StationID: "edge.rc", OrderType: dispatch.OrderTypeRetrieve,
		Status: dispatch.StatusQueued, Quantity: 1, DeliveryNode: node.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(known), "create the order the edge knows")
	unknown := &orders.Order{
		EdgeUUID: "rc-unknown", StationID: "edge.rc", OrderType: dispatch.OrderTypeRetrieveEmpty,
		Status: dispatch.StatusQueued, Quantity: 1, DeliveryNode: node.Name, SourceNode: "EMPTY-MARKET",
	}
	testutil.MustNoErr(t, db.CreateOrder(unknown), "create the order the edge lost")

	cap := &capturingResponder{}
	svc := NewCoreDataService(db, cap)
	env, req := edgeAsking("edge.rc", "rc-known")
	svc.HandleOrderStatusRequest(env, req)

	resp, ok := cap.payload.(*protocol.OrderStatusResponse)
	if !ok {
		t.Fatalf("replied with %T, want *protocol.OrderStatusResponse", cap.payload)
	}
	if len(resp.Orders) != 1 || resp.Orders[0].OrderUUID != "rc-known" || !resp.Orders[0].Found {
		t.Errorf("snapshots = %+v, want exactly the one asked for", resp.Orders)
	}
	if len(resp.Unlisted) != 1 {
		t.Fatalf("unlisted = %d orders, want 1 — the edge has no row for rc-unknown and will never get one otherwise", len(resp.Unlisted))
	}
	got := resp.Unlisted[0]
	if got.OrderUUID != "rc-unknown" {
		t.Errorf("unlisted order = %q, want rc-unknown", got.OrderUUID)
	}
	if got.DeliveryNode != node.Name || got.SourceNode != "EMPTY-MARKET" {
		t.Errorf("unlisted order nodes = %q -> %q, want EMPTY-MARKET -> %s", got.SourceNode, got.DeliveryNode, node.Name)
	}
	if !got.RetrieveEmpty {
		t.Error("retrieve_empty not carried; the Edge keeps it as its own column and would store the wrong kind of order")
	}
}

// TestOrderReconcile_NeverOffersAnotherStationsOrders scopes the heal. An Edge
// may only be healed with its own orders, and the station comes from the
// envelope rather than the request body — the one statement of who is asking
// that the sender cannot restate incorrectly.
func TestOrderReconcile_NeverOffersAnotherStationsOrders(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := &nodes.Node{Name: "RC2-W1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create node")

	mine := &orders.Order{
		EdgeUUID: "rc2-mine", StationID: "edge.mine", OrderType: dispatch.OrderTypeRetrieve,
		Status: dispatch.StatusQueued, Quantity: 1, DeliveryNode: node.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(mine), "create own order")
	theirs := &orders.Order{
		EdgeUUID: "rc2-theirs", StationID: "edge.theirs", OrderType: dispatch.OrderTypeRetrieve,
		Status: dispatch.StatusQueued, Quantity: 1, DeliveryNode: node.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(theirs), "create another station's order")

	cap := &capturingResponder{}
	svc := NewCoreDataService(db, cap)
	env, req := edgeAsking("edge.mine")
	svc.HandleOrderStatusRequest(env, req)

	resp := cap.payload.(*protocol.OrderStatusResponse)
	for _, p := range resp.Unlisted {
		if p.OrderUUID == "rc2-theirs" {
			t.Error("offered edge.mine an order belonging to edge.theirs")
		}
	}
	if len(resp.Unlisted) != 1 || resp.Unlisted[0].OrderUUID != "rc2-mine" {
		t.Errorf("unlisted = %+v, want exactly rc2-mine", resp.Unlisted)
	}
}

// TestOrderReconcile_AnEdgeWithNoOrdersStillGetsHealed is the case the old
// early-return made unreachable. An Edge that lost every row asks about nothing,
// and it is exactly the Edge most in need of an answer.
func TestOrderReconcile_AnEdgeWithNoOrdersStillGetsHealed(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := &nodes.Node{Name: "RC3-W1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create node")
	o := &orders.Order{
		EdgeUUID: "rc3-orphan", StationID: "edge.wiped", OrderType: dispatch.OrderTypeRetrieveEmpty,
		Status: dispatch.StatusQueued, Quantity: 1, DeliveryNode: node.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")

	cap := &capturingResponder{}
	svc := NewCoreDataService(db, cap)
	env, req := edgeAsking("edge.wiped") // asks about nothing at all
	svc.HandleOrderStatusRequest(env, req)

	resp := cap.payload.(*protocol.OrderStatusResponse)
	if len(resp.Unlisted) != 1 || resp.Unlisted[0].OrderUUID != "rc3-orphan" {
		t.Errorf("unlisted = %+v, want rc3-orphan — an edge with nothing to ask about is the one most likely to be missing an order", resp.Unlisted)
	}
}

// TestOrderReconcile_TerminalOrdersAreNotOffered keeps the heal from resurrecting
// finished work. A completed order is not something the Edge is missing; pushing
// it back would put closed jobs on the board every reconcile.
func TestOrderReconcile_TerminalOrdersAreNotOffered(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := &nodes.Node{Name: "RC4-W1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create node")
	done := &orders.Order{
		EdgeUUID: "rc4-done", StationID: "edge.rc4", OrderType: dispatch.OrderTypeRetrieve,
		Status: dispatch.StatusConfirmed, Quantity: 1, DeliveryNode: node.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(done), "create finished order")

	cap := &capturingResponder{}
	svc := NewCoreDataService(db, cap)
	env, req := edgeAsking("edge.rc4")
	svc.HandleOrderStatusRequest(env, req)

	resp := cap.payload.(*protocol.OrderStatusResponse)
	if len(resp.Unlisted) != 0 {
		t.Errorf("unlisted = %+v, want none — a finished order is not something the edge is missing", resp.Unlisted)
	}
}
