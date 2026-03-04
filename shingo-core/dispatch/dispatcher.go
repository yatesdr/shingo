package dispatch

import (
	"fmt"
	"log"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/store"
)

type Dispatcher struct {
	db            *store.DB
	backend       fleet.Backend
	emitter       Emitter
	stationID     string
	dispatchTopic string
	DebugLog      func(string, ...any)
}

func NewDispatcher(db *store.DB, backend fleet.Backend, emitter Emitter, stationID, dispatchTopic string) *Dispatcher {
	return &Dispatcher{
		db:            db,
		backend:       backend,
		emitter:       emitter,
		stationID:     stationID,
		dispatchTopic: dispatchTopic,
	}
}

func (d *Dispatcher) dbg(format string, args ...any) {
	if fn := d.DebugLog; fn != nil {
		fn(format, args...)
	}
}

func (d *Dispatcher) coreAddress() protocol.Address {
	return protocol.Address{Role: protocol.RoleCore, Station: d.stationID}
}

// HandleOrderRequest processes a new order from ShinGo Edge.
func (d *Dispatcher) HandleOrderRequest(env *protocol.Envelope, p *protocol.OrderRequest) {
	stationID := env.Src.Station
	d.dbg("order request: station=%s uuid=%s type=%s payload_type=%s delivery=%s pickup=%s",
		stationID, p.OrderUUID, p.OrderType, p.PayloadTypeCode, p.DeliveryNode, p.PickupNode)

	// Create order record
	order := &store.Order{
		EdgeUUID:     p.OrderUUID,
		StationID:     stationID,
		OrderType:    p.OrderType,
		Status:       StatusPending,
		Quantity:     p.Quantity,
		PickupNode:   p.PickupNode,
		DeliveryNode: p.DeliveryNode,
		Priority:     p.Priority,
		PayloadDesc:  p.PayloadDesc,
	}

	// Resolve payload type (optional — manual orders may not specify one)
	if p.PayloadTypeCode != "" {
		pt, err := d.db.GetPayloadTypeByName(p.PayloadTypeCode)
		if err != nil {
			log.Printf("dispatch: payload type %q not found: %v", p.PayloadTypeCode, err)
			d.dbg("error: payload type %q not found: %v", p.PayloadTypeCode, err)
			d.sendError(env, p.OrderUUID, "payload_type_error", fmt.Sprintf("payload type %q not found", p.PayloadTypeCode))
			return
		}
		order.PayloadTypeID = &pt.ID
	}

	// Validate destination node exists
	if p.DeliveryNode != "" {
		_, err := d.db.GetNodeByName(p.DeliveryNode)
		if err != nil {
			log.Printf("dispatch: delivery node %q not found: %v", p.DeliveryNode, err)
			d.dbg("error: delivery node %q not found: %v", p.DeliveryNode, err)
			d.sendError(env, p.OrderUUID, "invalid_node", fmt.Sprintf("delivery node %q not found", p.DeliveryNode))
			return
		}
	}

	if err := d.db.CreateOrder(order); err != nil {
		log.Printf("dispatch: create order: %v", err)
		d.sendError(env, p.OrderUUID, "internal_error", err.Error())
		return
	}
	d.db.UpdateOrderStatus(order.ID, StatusPending, "order received")

	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, p.OrderType, p.PayloadTypeCode, p.DeliveryNode)

	switch p.OrderType {
	case OrderTypeRetrieve:
		d.handleRetrieve(order, env, p.PayloadTypeCode)
	case OrderTypeMove:
		d.handleMove(order, env, p.PayloadTypeCode)
	case OrderTypeStore:
		d.handleStore(order, env)
	default:
		log.Printf("dispatch: unknown order type %q", p.OrderType)
		d.failOrder(order, env, "unknown_type", fmt.Sprintf("unknown order type: %s", p.OrderType))
	}
}

func (d *Dispatcher) handleRetrieve(order *store.Order, env *protocol.Envelope, payloadTypeCode string) {
	d.db.UpdateOrderStatus(order.ID, StatusSourcing, "finding source")

	// FIFO source selection for payloads
	source, err := d.db.FindSourcePayloadFIFO(payloadTypeCode)
	if err != nil {
		d.dbg("retrieve: no source payload for type %s", payloadTypeCode)
		d.failOrder(order, env, "no_source", fmt.Sprintf("no source payload found for type %s", payloadTypeCode))
		return
	}
	d.dbg("retrieve: FIFO source payload=%d type=%s node=%v", source.ID, payloadTypeCode, source.NodeID)

	// Claim the payload to prevent double-dispatch
	if err := d.db.ClaimPayload(source.ID, order.ID); err != nil {
		d.failOrder(order, env, "claim_failed", err.Error())
		return
	}

	// Get node details for vendor locations
	sourceNode, err := d.db.GetNode(*source.NodeID)
	if err != nil {
		d.failOrder(order, env, "node_error", err.Error())
		return
	}

	order.PickupNode = sourceNode.Name
	d.db.UpdateOrderPickupNode(order.ID, sourceNode.Name)

	destNode, err := d.db.GetNodeByName(order.DeliveryNode)
	if err != nil {
		d.failOrder(order, env, "node_error", err.Error())
		return
	}

	d.dispatchToFleet(order, env, sourceNode, destNode)
}

func (d *Dispatcher) handleMove(order *store.Order, env *protocol.Envelope, payloadTypeCode string) {
	d.db.UpdateOrderStatus(order.ID, StatusSourcing, "validating move")

	if order.PickupNode == "" {
		d.failOrder(order, env, "missing_pickup", "move order requires pickup_node")
		return
	}

	pickupNode, err := d.db.GetNodeByName(order.PickupNode)
	if err != nil {
		d.failOrder(order, env, "invalid_node", fmt.Sprintf("pickup node %q not found", order.PickupNode))
		return
	}

	// Validate unclaimed payload of requested type exists at pickup node
	if payloadTypeCode != "" {
		payloads, _ := d.db.ListPayloadsByNode(pickupNode.ID)
		found := false
		for _, p := range payloads {
			if p.PayloadTypeName == payloadTypeCode && p.ClaimedBy == nil {
				found = true
				if err := d.db.ClaimPayload(p.ID, order.ID); err == nil {
					d.dbg("move: claimed payload=%d type=%s at %s", p.ID, payloadTypeCode, order.PickupNode)
					break
				}
			}
		}
		if !found {
			d.dbg("move: no unclaimed %s payload at %s", payloadTypeCode, order.PickupNode)
			d.failOrder(order, env, "no_payload", fmt.Sprintf("no unclaimed %s payload at %s", payloadTypeCode, order.PickupNode))
			return
		}
	}

	d.db.UpdateOrderPickupNode(order.ID, pickupNode.Name)

	destNode, err := d.db.GetNodeByName(order.DeliveryNode)
	if err != nil {
		d.failOrder(order, env, "node_error", err.Error())
		return
	}

	d.dispatchToFleet(order, env, pickupNode, destNode)
}

func (d *Dispatcher) handleStore(order *store.Order, env *protocol.Envelope) {
	d.db.UpdateOrderStatus(order.ID, StatusSourcing, "finding storage destination")

	var payloadTypeID int64
	if order.PayloadTypeID != nil {
		payloadTypeID = *order.PayloadTypeID
	}

	destNode, err := d.db.FindStorageDestinationForPayload(payloadTypeID)
	if err != nil {
		d.dbg("store: no available storage node")
		d.failOrder(order, env, "no_storage", "no available storage node found")
		return
	}
	d.dbg("store: selected destination=%s for order %d", destNode.Name, order.ID)
	order.DeliveryNode = destNode.Name
	d.db.UpdateOrderDeliveryNode(order.ID, destNode.Name)

	// Pickup is the requesting line
	var pickupNode *store.Node
	if order.PickupNode != "" {
		pickupNode, err = d.db.GetNodeByName(order.PickupNode)
		if err != nil {
			d.failOrder(order, env, "invalid_node", fmt.Sprintf("pickup node %q not found", order.PickupNode))
			return
		}
	} else if order.DeliveryNode != "" {
		// Use delivery_node as source for store ops (line-side -> storage)
		pickupNode, err = d.db.GetNodeByName(order.DeliveryNode)
		if err != nil {
			d.failOrder(order, env, "invalid_node", fmt.Sprintf("node %q not found", order.DeliveryNode))
			return
		}
	}

	if pickupNode == nil {
		d.failOrder(order, env, "missing_pickup", "store order requires a pickup location")
		return
	}

	d.db.UpdateOrderPickupNode(order.ID, pickupNode.Name)

	d.dispatchToFleet(order, env, pickupNode, destNode)

}

func (d *Dispatcher) dispatchToFleet(order *store.Order, env *protocol.Envelope, sourceNode, destNode *store.Node) {
	vendorOrderID := fmt.Sprintf("sg-%d-%s", order.ID, uuid.New().String()[:8])

	req := fleet.TransportOrderRequest{
		OrderID:    vendorOrderID,
		ExternalID: order.EdgeUUID,
		FromLoc:    sourceNode.VendorLocation,
		ToLoc:      destNode.VendorLocation,
		Priority:   order.Priority,
	}

	d.dbg("fleet dispatch: order=%d vendor_id=%s from=%s(%s) to=%s(%s) priority=%d",
		order.ID, vendorOrderID, sourceNode.Name, sourceNode.VendorLocation, destNode.Name, destNode.VendorLocation, order.Priority)

	if _, err := d.backend.CreateTransportOrder(req); err != nil {
		log.Printf("dispatch: fleet create order failed: %v", err)
		d.dbg("fleet dispatch failed: %v", err)
		d.failOrder(order, env, "fleet_failed", err.Error())
		return
	}

	log.Printf("dispatch: order %d dispatched as %s (%s -> %s)", order.ID, vendorOrderID, sourceNode.Name, destNode.Name)
	d.dbg("fleet dispatch ok: order=%d vendor_id=%s", order.ID, vendorOrderID)

	d.db.UpdateOrderVendor(order.ID, vendorOrderID, "CREATED", "")
	d.db.UpdateOrderStatus(order.ID, StatusDispatched, fmt.Sprintf("vendor order %s created", vendorOrderID))

	d.emitter.EmitOrderDispatched(order.ID, vendorOrderID, sourceNode.Name, destNode.Name)

	// Send ack to ShinGo Edge
	d.sendAck(env, order.EdgeUUID, order.ID, sourceNode.Name)
}

// HandleOrderCancel processes a cancellation request from ShinGo Edge.
func (d *Dispatcher) HandleOrderCancel(env *protocol.Envelope, p *protocol.OrderCancel) {
	stationID := env.Src.Station
	d.dbg("cancel request: station=%s uuid=%s reason=%s", stationID, p.OrderUUID, p.Reason)

	order, err := d.db.GetOrderByUUID(p.OrderUUID)
	if err != nil {
		log.Printf("dispatch: cancel order %s not found: %v", p.OrderUUID, err)
		return
	}

	// If dispatched to fleet, cancel
	if order.VendorOrderID != "" && order.Status != StatusConfirmed && order.Status != StatusFailed && order.Status != StatusCancelled {
		if err := d.backend.CancelOrder(order.VendorOrderID); err != nil {
			log.Printf("dispatch: cancel vendor order %s: %v", order.VendorOrderID, err)
			d.dbg("cancel fleet error: vendor_id=%s: %v", order.VendorOrderID, err)
		} else {
			d.dbg("cancel fleet ok: vendor_id=%s", order.VendorOrderID)
		}
	}

	// Unclaim inventory if applicable
	d.unclaimOrderPayloads(order.ID)

	d.db.UpdateOrderStatus(order.ID, StatusCancelled, p.Reason)

	d.emitter.EmitOrderCancelled(order.ID, order.EdgeUUID, stationID, p.Reason)

	// Send cancelled reply via protocol
	edgeAddr := protocol.Address{Role: protocol.RoleEdge, Station: stationID}
	reply, err := protocol.NewReply(protocol.TypeOrderCancelled, d.coreAddress(), edgeAddr, env.ID, &protocol.OrderCancelled{
		OrderUUID: p.OrderUUID,
		Reason:    p.Reason,
	})
	if err != nil {
		log.Printf("dispatch: build cancelled reply: %v", err)
		return
	}
	data, err := reply.Encode()
	if err != nil {
		log.Printf("dispatch: encode cancelled reply: %v", err)
		return
	}
	d.db.EnqueueOutbox(d.dispatchTopic, data, "order.cancelled", stationID)
}

// HandleOrderReceipt processes a delivery confirmation from ShinGo Edge.
func (d *Dispatcher) HandleOrderReceipt(env *protocol.Envelope, p *protocol.OrderReceipt) {
	stationID := env.Src.Station
	d.dbg("delivery receipt: station=%s uuid=%s type=%s count=%.1f", stationID, p.OrderUUID, p.ReceiptType, p.FinalCount)

	order, err := d.db.GetOrderByUUID(p.OrderUUID)
	if err != nil {
		log.Printf("dispatch: delivery receipt order %s not found: %v", p.OrderUUID, err)
		return
	}

	d.db.UpdateOrderStatus(order.ID, StatusConfirmed, fmt.Sprintf("receipt: %s, count: %.1f", p.ReceiptType, p.FinalCount))

	// Transition confirmed -> completed
	d.db.CompleteOrder(order.ID)
	d.emitter.EmitOrderCompleted(order.ID, order.EdgeUUID, stationID)
}

// HandleOrderRedirect processes a redirect request from ShinGo Edge.
func (d *Dispatcher) HandleOrderRedirect(env *protocol.Envelope, p *protocol.OrderRedirect) {
	d.dbg("redirect: uuid=%s new_dest=%s", p.OrderUUID, p.NewDeliveryNode)

	order, err := d.db.GetOrderByUUID(p.OrderUUID)
	if err != nil {
		log.Printf("dispatch: redirect order %s not found: %v", p.OrderUUID, err)
		return
	}

	// Cancel existing vendor order
	if order.VendorOrderID != "" {
		if err := d.backend.CancelOrder(order.VendorOrderID); err != nil {
			log.Printf("dispatch: cancel for redirect %s: %v", order.VendorOrderID, err)
		}
	}

	// Update destination
	newDest, err := d.db.GetNodeByName(p.NewDeliveryNode)
	if err != nil {
		log.Printf("dispatch: redirect dest %q not found: %v", p.NewDeliveryNode, err)
		d.sendError(env, p.OrderUUID, "invalid_node", fmt.Sprintf("redirect destination %q not found", p.NewDeliveryNode))
		return
	}

	order.DeliveryNode = p.NewDeliveryNode

	// Get source node for re-dispatch
	if order.PickupNode == "" {
		d.sendError(env, p.OrderUUID, "redirect_failed", "no source node for redirect")
		return
	}
	sourceNode, err := d.db.GetNodeByName(order.PickupNode)
	if err != nil {
		d.sendError(env, p.OrderUUID, "redirect_failed", err.Error())
		return
	}

	d.db.UpdateOrderStatus(order.ID, StatusSourcing, fmt.Sprintf("redirecting to %s", p.NewDeliveryNode))
	d.dispatchToFleet(order, env, sourceNode, newDest)
}

// HandleOrderStorageWaybill processes a storage waybill from ShinGo Edge.
func (d *Dispatcher) HandleOrderStorageWaybill(env *protocol.Envelope, p *protocol.OrderStorageWaybill) {
	stationID := env.Src.Station
	d.dbg("storage waybill: station=%s uuid=%s type=%s pickup=%s", stationID, p.OrderUUID, p.OrderType, p.PickupNode)

	order := &store.Order{
		EdgeUUID:    p.OrderUUID,
		StationID:    stationID,
		OrderType:   p.OrderType,
		Status:      StatusPending,
		PickupNode:  p.PickupNode,
		PayloadDesc: p.PayloadDesc,
	}

	if err := d.db.CreateOrder(order); err != nil {
		log.Printf("dispatch: create store order: %v", err)
		d.sendError(env, p.OrderUUID, "internal_error", err.Error())
		return
	}
	d.db.UpdateOrderStatus(order.ID, StatusPending, "store order received")

	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, p.OrderType, "", p.PickupNode)

	d.handleStore(order, env)
}

func (d *Dispatcher) failOrder(order *store.Order, env *protocol.Envelope, errorCode, detail string) {
	stationID := env.Src.Station
	d.db.UpdateOrderStatus(order.ID, StatusFailed, detail)
	d.unclaimOrderPayloads(order.ID)
	d.emitter.EmitOrderFailed(order.ID, order.EdgeUUID, stationID, errorCode, detail)
	d.sendError(env, order.EdgeUUID, errorCode, detail)
}

func (d *Dispatcher) unclaimOrderPayloads(orderID int64) {
	// Collect IDs first, then unclaim — avoids holding rows cursor during Exec (SQLite deadlock)
	rows, err := d.db.Query(d.db.Q(`SELECT id FROM payloads WHERE claimed_by=?`), orderID)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		d.db.UnclaimPayload(id)
	}
}

func (d *Dispatcher) sendAck(env *protocol.Envelope, orderUUID string, shingoOrderID int64, sourceNode string) {
	stationID := env.Src.Station
	edgeAddr := protocol.Address{Role: protocol.RoleEdge, Station: stationID}
	reply, err := protocol.NewReply(protocol.TypeOrderAck, d.coreAddress(), edgeAddr, env.ID, &protocol.OrderAck{
		OrderUUID:     orderUUID,
		ShingoOrderID: shingoOrderID,
		SourceNode:    sourceNode,
	})
	if err != nil {
		log.Printf("dispatch: build ack reply: %v", err)
		return
	}
	data, err := reply.Encode()
	if err != nil {
		log.Printf("dispatch: encode ack reply: %v", err)
		return
	}
	d.dbg("sendAck: uuid=%s shingo_id=%d source=%s", orderUUID, shingoOrderID, sourceNode)
	d.db.EnqueueOutbox(d.dispatchTopic, data, "order.ack", stationID)
}

func (d *Dispatcher) sendError(env *protocol.Envelope, orderUUID, errorCode, detail string) {
	stationID := env.Src.Station
	edgeAddr := protocol.Address{Role: protocol.RoleEdge, Station: stationID}
	reply, err := protocol.NewReply(protocol.TypeOrderError, d.coreAddress(), edgeAddr, env.ID, &protocol.OrderError{
		OrderUUID: orderUUID,
		ErrorCode: errorCode,
		Detail:    detail,
	})
	if err != nil {
		log.Printf("dispatch: build error reply: %v", err)
		return
	}
	data, err := reply.Encode()
	if err != nil {
		log.Printf("dispatch: encode error reply: %v", err)
		return
	}
	d.dbg("sendError: uuid=%s code=%s detail=%s", orderUUID, errorCode, detail)
	d.db.EnqueueOutbox(d.dispatchTopic, data, "order.error", stationID)
}
