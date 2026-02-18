package dispatch

import (
	"fmt"
	"log"

	"github.com/google/uuid"

	"warpath/messaging"
	"warpath/rds"
	"warpath/store"
)

type Dispatcher struct {
	db        *store.DB
	rdsClient *rds.Client
	emitter   Emitter
	factoryID string
	dispatchTopicPrefix string
}

func NewDispatcher(db *store.DB, rdsClient *rds.Client, emitter Emitter, factoryID, dispatchTopicPrefix string) *Dispatcher {
	return &Dispatcher{
		db:                  db,
		rdsClient:           rdsClient,
		emitter:             emitter,
		factoryID:           factoryID,
		dispatchTopicPrefix: dispatchTopicPrefix,
	}
}

// HandleOrderRequest processes a new order from WarDrop.
func (d *Dispatcher) HandleOrderRequest(env *messaging.Envelope, req messaging.OrderRequest) {
	// Create order record
	order := &store.Order{
		WardropUUID:  req.OrderUUID,
		ClientID:     env.ClientID,
		FactoryID:    env.FactoryID,
		OrderType:    req.OrderType,
		Status:       StatusPending,
		Quantity:     req.Quantity,
		PickupNode:   req.PickupNode,
		DeliveryNode: req.DeliveryNode,
		Priority:     req.Priority,
		PayloadDesc:  req.PayloadDesc,
	}

	// Resolve payload type
	pt, err := d.db.GetPayloadTypeByName(req.PayloadTypeCode)
	if err != nil {
		log.Printf("dispatch: payload type %q not found: %v", req.PayloadTypeCode, err)
		d.sendError(env.ClientID, req.OrderUUID, "payload_type_error", fmt.Sprintf("payload type %q not found", req.PayloadTypeCode))
		return
	}
	order.PayloadTypeID = &pt.ID

	// Resolve destination node
	if req.DeliveryNode != "" {
		destNode, err := d.db.GetNodeByName(req.DeliveryNode)
		if err != nil {
			log.Printf("dispatch: delivery node %q not found: %v", req.DeliveryNode, err)
			d.sendError(env.ClientID, req.OrderUUID, "invalid_node", fmt.Sprintf("delivery node %q not found", req.DeliveryNode))
			return
		}
		order.DestNodeID = &destNode.ID
	}

	if err := d.db.CreateOrder(order); err != nil {
		log.Printf("dispatch: create order: %v", err)
		d.sendError(env.ClientID, req.OrderUUID, "internal_error", err.Error())
		return
	}
	d.db.UpdateOrderStatus(order.ID, StatusPending, "order received")

	d.emitter.EmitOrderReceived(order.ID, order.WardropUUID, env.ClientID, req.OrderType, req.PayloadTypeCode, req.DeliveryNode)

	switch req.OrderType {
	case OrderTypeRetrieve:
		d.handleRetrieve(order, env.ClientID, req.PayloadTypeCode)
	case OrderTypeMove:
		d.handleMove(order, env.ClientID, req.PayloadTypeCode)
	case OrderTypeStore:
		d.handleStore(order, env.ClientID)
	default:
		log.Printf("dispatch: unknown order type %q", req.OrderType)
		d.failOrder(order, env.ClientID, "unknown_type", fmt.Sprintf("unknown order type: %s", req.OrderType))
	}
}

func (d *Dispatcher) handleRetrieve(order *store.Order, clientID, payloadTypeCode string) {
	d.db.UpdateOrderStatus(order.ID, StatusSourcing, "finding source")

	// FIFO source selection for payloads
	source, err := d.db.FindSourcePayloadFIFO(payloadTypeCode)
	if err != nil {
		d.failOrder(order, clientID, "no_source", fmt.Sprintf("no source payload found for type %s", payloadTypeCode))
		return
	}

	// Claim the payload to prevent double-dispatch
	if err := d.db.ClaimPayload(source.ID, order.ID); err != nil {
		d.failOrder(order, clientID, "claim_failed", err.Error())
		return
	}

	order.SourceNodeID = source.NodeID
	d.db.UpdateOrderSourceNode(order.ID, *source.NodeID)

	// Get node details for RDS locations
	sourceNode, err := d.db.GetNode(*source.NodeID)
	if err != nil {
		d.failOrder(order, clientID, "node_error", err.Error())
		return
	}

	destNode, err := d.db.GetNode(*order.DestNodeID)
	if err != nil {
		d.failOrder(order, clientID, "node_error", err.Error())
		return
	}

	d.dispatchToRDS(order, clientID, sourceNode, destNode)
}

func (d *Dispatcher) handleMove(order *store.Order, clientID, payloadTypeCode string) {
	d.db.UpdateOrderStatus(order.ID, StatusSourcing, "validating move")

	if order.PickupNode == "" {
		d.failOrder(order, clientID, "missing_pickup", "move order requires pickup_node")
		return
	}

	pickupNode, err := d.db.GetNodeByName(order.PickupNode)
	if err != nil {
		d.failOrder(order, clientID, "invalid_node", fmt.Sprintf("pickup node %q not found", order.PickupNode))
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
					break
				}
			}
		}
		if !found {
			d.failOrder(order, clientID, "no_payload", fmt.Sprintf("no unclaimed %s payload at %s", payloadTypeCode, order.PickupNode))
			return
		}
	}

	order.SourceNodeID = &pickupNode.ID
	d.db.UpdateOrderSourceNode(order.ID, pickupNode.ID)

	destNode, err := d.db.GetNode(*order.DestNodeID)
	if err != nil {
		d.failOrder(order, clientID, "node_error", err.Error())
		return
	}

	d.dispatchToRDS(order, clientID, pickupNode, destNode)
}

func (d *Dispatcher) handleStore(order *store.Order, clientID string) {
	d.db.UpdateOrderStatus(order.ID, StatusSourcing, "finding storage destination")

	var payloadTypeID int64
	if order.PayloadTypeID != nil {
		payloadTypeID = *order.PayloadTypeID
	}

	destNode, err := d.db.FindStorageDestinationForPayload(payloadTypeID)
	if err != nil {
		d.failOrder(order, clientID, "no_storage", "no available storage node found")
		return
	}
	order.DestNodeID = &destNode.ID

	// Pickup is the requesting line
	var pickupNode *store.Node
	if order.PickupNode != "" {
		pickupNode, err = d.db.GetNodeByName(order.PickupNode)
		if err != nil {
			d.failOrder(order, clientID, "invalid_node", fmt.Sprintf("pickup node %q not found", order.PickupNode))
			return
		}
	} else if order.DeliveryNode != "" {
		// Use delivery_node as source for store ops (line-side -> storage)
		pickupNode, err = d.db.GetNodeByName(order.DeliveryNode)
		if err != nil {
			d.failOrder(order, clientID, "invalid_node", fmt.Sprintf("node %q not found", order.DeliveryNode))
			return
		}
	}

	if pickupNode == nil {
		d.failOrder(order, clientID, "missing_pickup", "store order requires a pickup location")
		return
	}

	order.SourceNodeID = &pickupNode.ID
	d.db.UpdateOrderSourceNode(order.ID, pickupNode.ID)

	d.dispatchToRDS(order, clientID, pickupNode, destNode)
}

func (d *Dispatcher) dispatchToRDS(order *store.Order, clientID string, sourceNode, destNode *store.Node) {
	rdsOrderID := fmt.Sprintf("wp-%d-%s", order.ID, uuid.New().String()[:8])

	req := &rds.SetJoinOrderRequest{
		ID:         rdsOrderID,
		ExternalID: order.WardropUUID,
		FromLoc:    sourceNode.RDSLocation,
		ToLoc:      destNode.RDSLocation,
		Priority:   order.Priority,
	}

	if err := d.rdsClient.CreateJoinOrder(req); err != nil {
		log.Printf("dispatch: RDS create order failed: %v", err)
		d.failOrder(order, clientID, "rds_failed", err.Error())
		return
	}

	d.db.UpdateOrderRDS(order.ID, rdsOrderID, "CREATED", "")
	d.db.UpdateOrderStatus(order.ID, StatusDispatched, fmt.Sprintf("RDS order %s created", rdsOrderID))

	d.emitter.EmitOrderDispatched(order.ID, rdsOrderID, sourceNode.Name, destNode.Name)

	// Send ack to WarDrop
	d.sendAck(clientID, order.WardropUUID, order.ID, sourceNode.Name)
}

// HandleOrderCancel processes a cancellation request from WarDrop.
func (d *Dispatcher) HandleOrderCancel(env *messaging.Envelope, req messaging.OrderCancel) {
	order, err := d.db.GetOrderByUUID(req.OrderUUID)
	if err != nil {
		log.Printf("dispatch: cancel order %s not found: %v", req.OrderUUID, err)
		return
	}

	// If dispatched to RDS, terminate
	if order.RDSOrderID != "" && order.Status != StatusCompleted && order.Status != StatusFailed && order.Status != StatusCancelled {
		if err := d.rdsClient.TerminateOrder(&rds.TerminateRequest{
			ID:             order.RDSOrderID,
			DisableVehicle: false,
		}); err != nil {
			log.Printf("dispatch: terminate RDS order %s: %v", order.RDSOrderID, err)
		}
	}

	// Unclaim inventory if applicable
	d.unclaimOrderPayloads(order.ID)

	d.db.UpdateOrderStatus(order.ID, StatusCancelled, req.Reason)

	d.emitter.EmitOrderCancelled(order.ID, order.WardropUUID, env.ClientID, req.Reason)

	// Send cancelled reply
	reply := messaging.NewEnvelope("cancelled", env.ClientID, d.factoryID, messaging.CancelledReply{
		OrderUUID: req.OrderUUID,
		Reason:    req.Reason,
	})
	data, _ := reply.Encode()
	topic := messaging.DispatchTopic(d.dispatchTopicPrefix, env.ClientID)
	d.db.EnqueueOutbox(topic, data, "cancelled", env.ClientID)
}

// HandleDeliveryReceipt processes a delivery confirmation from WarDrop.
func (d *Dispatcher) HandleDeliveryReceipt(env *messaging.Envelope, req messaging.DeliveryReceipt) {
	order, err := d.db.GetOrderByUUID(req.OrderUUID)
	if err != nil {
		log.Printf("dispatch: delivery receipt order %s not found: %v", req.OrderUUID, err)
		return
	}

	d.db.UpdateOrderStatus(order.ID, StatusConfirmed, fmt.Sprintf("receipt: %s, count: %.1f", req.ReceiptType, req.FinalCount))

	// Transition confirmed -> completed
	d.db.CompleteOrder(order.ID)
	d.emitter.EmitOrderCompleted(order.ID, order.WardropUUID, env.ClientID)
}

// HandleRedirectRequest processes a redirect request from WarDrop.
func (d *Dispatcher) HandleRedirectRequest(env *messaging.Envelope, req messaging.RedirectRequest) {
	order, err := d.db.GetOrderByUUID(req.OrderUUID)
	if err != nil {
		log.Printf("dispatch: redirect order %s not found: %v", req.OrderUUID, err)
		return
	}

	// Terminate existing RDS order
	if order.RDSOrderID != "" {
		if err := d.rdsClient.TerminateOrder(&rds.TerminateRequest{
			ID:             order.RDSOrderID,
			DisableVehicle: false,
		}); err != nil {
			log.Printf("dispatch: terminate for redirect %s: %v", order.RDSOrderID, err)
		}
	}

	// Update destination
	newDest, err := d.db.GetNodeByName(req.NewDeliveryNode)
	if err != nil {
		log.Printf("dispatch: redirect dest %q not found: %v", req.NewDeliveryNode, err)
		d.sendError(env.ClientID, req.OrderUUID, "invalid_node", fmt.Sprintf("redirect destination %q not found", req.NewDeliveryNode))
		return
	}

	order.DestNodeID = &newDest.ID
	order.DeliveryNode = req.NewDeliveryNode

	// Get source node for re-dispatch
	if order.SourceNodeID == nil {
		d.sendError(env.ClientID, req.OrderUUID, "redirect_failed", "no source node for redirect")
		return
	}
	sourceNode, err := d.db.GetNode(*order.SourceNodeID)
	if err != nil {
		d.sendError(env.ClientID, req.OrderUUID, "redirect_failed", err.Error())
		return
	}

	d.db.UpdateOrderStatus(order.ID, StatusSourcing, fmt.Sprintf("redirecting to %s", req.NewDeliveryNode))
	d.dispatchToRDS(order, env.ClientID, sourceNode, newDest)
}

func (d *Dispatcher) failOrder(order *store.Order, clientID, errorCode, detail string) {
	d.db.UpdateOrderStatus(order.ID, StatusFailed, detail)
	d.unclaimOrderPayloads(order.ID)
	d.emitter.EmitOrderFailed(order.ID, order.WardropUUID, clientID, errorCode, detail)
	d.sendError(clientID, order.WardropUUID, errorCode, detail)
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

func (d *Dispatcher) sendAck(clientID, orderUUID string, warpathOrderID int64, sourceNode string) {
	reply := messaging.NewEnvelope("ack", clientID, d.factoryID, messaging.AckReply{
		OrderUUID:      orderUUID,
		WarpathOrderID: warpathOrderID,
		SourceNode:     sourceNode,
	})
	data, _ := reply.Encode()
	topic := messaging.DispatchTopic(d.dispatchTopicPrefix, clientID)
	d.db.EnqueueOutbox(topic, data, "ack", clientID)
}

func (d *Dispatcher) sendError(clientID, orderUUID, errorCode, detail string) {
	reply := messaging.NewEnvelope("error", clientID, d.factoryID, messaging.ErrorReply{
		OrderUUID: orderUUID,
		ErrorCode: errorCode,
		Detail:    detail,
	})
	data, _ := reply.Encode()
	topic := messaging.DispatchTopic(d.dispatchTopicPrefix, clientID)
	d.db.EnqueueOutbox(topic, data, "error", clientID)
}
