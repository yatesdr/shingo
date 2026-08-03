package engine

import (
	"database/sql"
	"errors"
	"fmt"

	"shingo/protocol"
	"shingoedge/store/orders"
)

// order_projection.go — applying an order row Core authored.
//
// Core is becoming an author of orders rather than only a fulfiller of Edge
// requests. An order the Edge never created has no row here, which means the
// operator cannot see it on the board, the delivery handler cannot bind its bin
// (it falls back to a bind-only event and returns an error), and nothing can
// explain why a robot is coming. The projection gives the Edge the row.
//
// The push is not the only way a projection arrives, and building it as if it
// were would be a mistake: the Core → Edge outbox drops a message permanently
// once it is past its retry limit. A projection that never lands is normal. The
// order status reconcile carries the same rows back for anything missing, and
// calls straight into here.

// ApplyOrderProjection writes a Core-authored order into the Edge's own orders
// table. Returns whether the row was newly created.
//
// IDEMPOTENT. The same projection may arrive twice — once pushed, once through
// the reconcile — and the second one must change nothing. That is the store
// layer's job (UpsertProjection); this resolves what the store cannot.
//
// IT ENQUEUES NOTHING BACK TO CORE. Nothing here calls the order sender, and
// that is deliberate rather than incidental: an Edge that answered a projection
// by telling Core about the order would loop. The provenance test asserts zero
// outbox rows rather than trusting the absence of a call.
func (e *Engine) ApplyOrderProjection(p protocol.OrderProjection) (created bool, err error) {
	if p.OrderUUID == "" {
		return false, errors.New("order projection with no uuid")
	}

	row := orders.ProjectionRow{
		UUID:          p.OrderUUID,
		OrderType:     p.OrderType,
		Status:        p.Status,
		ProcessNodeID: e.resolveProjectionNode(p),
		RetrieveEmpty: p.RetrieveEmpty,
		Quantity:      p.Quantity,
		SourceNode:    p.SourceNode,
		DeliveryNode:  p.DeliveryNode,
		PayloadCode:   p.PayloadCode,
		QueueReason:   p.QueueReason,
		QueueCode:     p.QueueCode,
	}
	created, err = orders.UpsertProjection(e.db.DB, row)
	if err != nil {
		return false, fmt.Errorf("apply order projection %s: %w", p.OrderUUID, err)
	}
	e.debugFn.Log("order projection applied: uuid=%s type=%s status=%s delivery=%s created=%v",
		p.OrderUUID, p.OrderType, p.Status, p.DeliveryNode, created)
	return created, nil
}

// resolveProjectionNode turns Core's delivery node NAME into this Edge's own
// process_node id, which is what the operator board joins on.
//
// Core has never had that id — it is Edge-local — so the resolution has to
// happen here, and it can legitimately fail. A Core node with no matching Edge
// process node is a supported shape: a manual move to a storage location, or a
// node belonging to another station. Those get a null, exactly as a manually
// created order to the same place does today, and the order still appears in the
// orders list. Refusing the projection over it would throw away the row entirely
// to avoid a nullable column doing its job.
func (e *Engine) resolveProjectionNode(p protocol.OrderProjection) *int64 {
	if p.DeliveryNode == "" {
		return nil
	}
	node, err := e.db.GetProcessNodeByCoreNodeName(p.DeliveryNode)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			// A real read failure, not a miss. Worth a line: it means the row is
			// about to land unbound for a reason that is not "no such node here".
			e.debugFn.Log("order projection %s: resolving delivery node %q failed: %v",
				p.OrderUUID, p.DeliveryNode, err)
		}
		return nil
	}
	if node == nil {
		return nil
	}
	id := node.ID
	return &id
}
