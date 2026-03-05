package dispatch

import (
	"errors"
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
	resolver      NodeResolver
	laneLock      *LaneLock
	stationID     string
	dispatchTopic string
	DebugLog      func(string, ...any)
}

func NewDispatcher(db *store.DB, backend fleet.Backend, emitter Emitter, stationID, dispatchTopic string, resolver NodeResolver) *Dispatcher {
	return &Dispatcher{
		db:            db,
		backend:       backend,
		emitter:       emitter,
		resolver:      resolver,
		laneLock:      NewLaneLock(),
		stationID:     stationID,
		dispatchTopic: dispatchTopic,
	}
}

func (d *Dispatcher) dbg(format string, args ...any) {
	if fn := d.DebugLog; fn != nil {
		fn(format, args...)
	}
}

// HandleOrderRequest processes a new order from ShinGo Edge.
func (d *Dispatcher) HandleOrderRequest(env *protocol.Envelope, p *protocol.OrderRequest) {
	stationID := env.Src.Station
	styleCode := p.PayloadTypeCode
	if styleCode == "" {
		styleCode = p.StyleCode
	}
	d.dbg("order request: station=%s uuid=%s type=%s style=%s delivery=%s pickup=%s",
		stationID, p.OrderUUID, p.OrderType, styleCode, p.DeliveryNode, p.PickupNode)

	// Create order record
	order := &store.Order{
		EdgeUUID:     p.OrderUUID,
		StationID:    stationID,
		OrderType:    p.OrderType,
		Status:       StatusPending,
		Quantity:     p.Quantity,
		PickupNode:   p.PickupNode,
		DeliveryNode: p.DeliveryNode,
		Priority:     p.Priority,
		PayloadDesc:  p.PayloadDesc,
	}

	// Resolve payload style (optional — manual orders may not specify one)
	if styleCode != "" {
		ps, err := d.db.GetPayloadStyleByName(styleCode)
		if err != nil {
			log.Printf("dispatch: payload style %q not found: %v", styleCode, err)
			d.dbg("error: payload style %q not found: %v", styleCode, err)
			d.sendError(env, p.OrderUUID, "style_error", fmt.Sprintf("payload style %q not found", styleCode))
			return
		}
		order.StyleID = &ps.ID
	}

	// Validate destination node exists; resolve synthetic nodes
	if p.DeliveryNode != "" {
		destNode, err := d.db.GetNodeByName(p.DeliveryNode)
		if err != nil {
			log.Printf("dispatch: delivery node %q not found: %v", p.DeliveryNode, err)
			d.dbg("error: delivery node %q not found: %v", p.DeliveryNode, err)
			d.sendError(env, p.OrderUUID, "invalid_node", fmt.Sprintf("delivery node %q not found", p.DeliveryNode))
			return
		}
		if destNode.IsSynthetic && d.resolver != nil {
			result, err := d.resolver.Resolve(destNode, p.OrderType, order.StyleID)
			if err != nil {
				d.dbg("synthetic resolution failed for %s: %v", p.DeliveryNode, err)
				d.sendError(env, p.OrderUUID, "resolution_failed", fmt.Sprintf("cannot resolve synthetic node %s: %v", p.DeliveryNode, err))
				return
			}
			d.dbg("resolved synthetic %s -> %s", p.DeliveryNode, result.Node.Name)
			order.DeliveryNode = result.Node.Name
		}
	}

	if err := d.db.CreateOrder(order); err != nil {
		log.Printf("dispatch: create order: %v", err)
		d.sendError(env, p.OrderUUID, "internal_error", err.Error())
		return
	}
	d.db.UpdateOrderStatus(order.ID, StatusPending, "order received")

	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, p.OrderType, styleCode, p.DeliveryNode)

	switch p.OrderType {
	case OrderTypeRetrieve:
		d.handleRetrieve(order, env, styleCode)
	case OrderTypeMove:
		d.handleMove(order, env, styleCode)
	case OrderTypeStore:
		d.handleStore(order, env)
	default:
		log.Printf("dispatch: unknown order type %q", p.OrderType)
		d.failOrder(order, env, "unknown_type", fmt.Sprintf("unknown order type: %s", p.OrderType))
	}
}

func (d *Dispatcher) handleRetrieve(order *store.Order, env *protocol.Envelope, styleCode string) {
	d.db.UpdateOrderStatus(order.ID, StatusSourcing, "finding source")

	var source *store.PayloadInstance
	var sourceNode *store.Node

	// Try group-aware resolution if a pickup node is specified and is an NGRP
	if order.PickupNode != "" && d.resolver != nil {
		pickupNode, err := d.db.GetNodeByName(order.PickupNode)
		if err == nil && pickupNode.IsSynthetic && pickupNode.NodeTypeCode == "NGRP" {
			result, err := d.resolver.Resolve(pickupNode, OrderTypeRetrieve, order.StyleID)
			if err != nil {
				// Check if buried — trigger reshuffle
				var buriedErr *BuriedError
				if errors.As(err, &buriedErr) {
					d.dbg("retrieve: instance %d buried in lane %d, planning reshuffle", buriedErr.Instance.ID, buriedErr.LaneID)
					d.handleBuriedReshuffle(order, env, buriedErr)
					return
				}
				d.dbg("retrieve: node group resolution failed for %s: %v", order.PickupNode, err)
				d.failOrder(order, env, "no_source", fmt.Sprintf("no source in node group %s: %v", order.PickupNode, err))
				return
			}
			source = result.Instance
			sourceNode, _ = d.db.GetNode(*source.NodeID)
		}
	}

	// Fallback: global FIFO source selection
	if source == nil {
		var err error
		source, err = d.db.FindSourceInstanceFIFO(styleCode)
		if err != nil {
			d.dbg("retrieve: no source instance for style %s", styleCode)
			d.failOrder(order, env, "no_source", fmt.Sprintf("no source instance found for style %s", styleCode))
			return
		}
		sourceNode, err = d.db.GetNode(*source.NodeID)
		if err != nil {
			d.failOrder(order, env, "node_error", err.Error())
			return
		}
	}

	d.dbg("retrieve: FIFO source instance=%d style=%s node=%s", source.ID, styleCode, sourceNode.Name)

	// Claim the instance to prevent double-dispatch
	if err := d.db.ClaimInstance(source.ID, order.ID); err != nil {
		d.failOrder(order, env, "claim_failed", err.Error())
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

// handleBuriedReshuffle plans and executes a reshuffle when a FIFO target is buried.
func (d *Dispatcher) handleBuriedReshuffle(order *store.Order, env *protocol.Envelope, buried *BuriedError) {
	// Check lane lock
	if d.laneLock.IsLocked(buried.LaneID) {
		d.failOrder(order, env, "lane_locked", fmt.Sprintf("lane %d is locked by another reshuffle", buried.LaneID))
		return
	}

	// Find the group (parent of lane)
	lane, err := d.db.GetNode(buried.LaneID)
	if err != nil || lane.ParentID == nil {
		d.failOrder(order, env, "reshuffle_error", "cannot determine node group for lane")
		return
	}

	plan, err := PlanReshuffle(d.db, buried.Instance, buried.Slot, lane, *lane.ParentID)
	if err != nil {
		d.failOrder(order, env, "reshuffle_error", fmt.Sprintf("cannot plan reshuffle: %v", err))
		return
	}

	// Lock the lane
	if !d.laneLock.TryLock(buried.LaneID, order.ID) {
		d.failOrder(order, env, "lane_locked", "lane locked concurrently")
		return
	}

	// Create compound order
	if err := d.CreateCompoundOrder(order, plan); err != nil {
		d.laneLock.Unlock(buried.LaneID)
		d.failOrder(order, env, "reshuffle_error", fmt.Sprintf("cannot create compound order: %v", err))
		return
	}

	d.db.UpdateOrderStatus(order.ID, StatusReshuffling, fmt.Sprintf("reshuffling lane — %d steps", len(plan.Steps)))
	d.dbg("retrieve: compound reshuffle created for order %d: %d steps", order.ID, len(plan.Steps))

	// Advance to first step
	d.AdvanceCompoundOrder(order.ID)
}

func (d *Dispatcher) handleMove(order *store.Order, env *protocol.Envelope, styleCode string) {
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

	// Validate unclaimed instance of requested style exists at pickup node
	if styleCode != "" {
		instances, _ := d.db.ListInstancesByNode(pickupNode.ID)
		claimed := false
		for _, p := range instances {
			if p.StyleName == styleCode && p.ClaimedBy == nil {
				if err := d.db.ClaimInstance(p.ID, order.ID); err == nil {
					d.dbg("move: claimed instance=%d style=%s at %s", p.ID, styleCode, order.PickupNode)
					claimed = true
					break
				}
			}
		}
		if !claimed {
			d.dbg("move: no unclaimed %s instance at %s", styleCode, order.PickupNode)
			d.failOrder(order, env, "no_payload", fmt.Sprintf("no unclaimed %s instance at %s", styleCode, order.PickupNode))
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

	var styleID int64
	if order.StyleID != nil {
		styleID = *order.StyleID
	}

	// Capture the original delivery node before overwriting — it may be the pickup source
	originalDeliveryNode := order.DeliveryNode

	destNode, err := d.db.FindStorageDestinationForInstance(styleID)
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
	} else if originalDeliveryNode != "" {
		// Use original delivery_node as source for store ops (line-side -> storage)
		pickupNode, err = d.db.GetNodeByName(originalDeliveryNode)
		if err != nil {
			d.failOrder(order, env, "invalid_node", fmt.Sprintf("node %q not found", originalDeliveryNode))
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
		FromLoc:    sourceNode.Name,
		ToLoc:      destNode.Name,
		Priority:   order.Priority,
	}

	d.dbg("fleet dispatch: order=%d vendor_id=%s from=%s to=%s priority=%d",
		order.ID, vendorOrderID, sourceNode.Name, destNode.Name, order.Priority)

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

// DispatchDirect dispatches an order to the fleet without a protocol envelope.
// Used for orders created internally (e.g. direct orders from the UI).
// Returns the vendor order ID on success.
func (d *Dispatcher) DispatchDirect(order *store.Order, sourceNode, destNode *store.Node) (string, error) {
	vendorOrderID := fmt.Sprintf("sg-%d-%s", order.ID, uuid.New().String()[:8])

	req := fleet.TransportOrderRequest{
		OrderID:    vendorOrderID,
		ExternalID: order.EdgeUUID,
		FromLoc:    sourceNode.Name,
		ToLoc:      destNode.Name,
		Priority:   order.Priority,
	}

	d.dbg("fleet dispatch (direct): order=%d vendor_id=%s from=%s to=%s",
		order.ID, vendorOrderID, sourceNode.Name, destNode.Name)

	if _, err := d.backend.CreateTransportOrder(req); err != nil {
		log.Printf("dispatch: fleet create order failed: %v", err)
		d.db.UpdateOrderStatus(order.ID, StatusFailed, err.Error())
		return "", err
	}

	d.db.UpdateOrderVendor(order.ID, vendorOrderID, "CREATED", "")
	d.db.UpdateOrderStatus(order.ID, StatusDispatched, fmt.Sprintf("vendor order %s created", vendorOrderID))
	d.emitter.EmitOrderDispatched(order.ID, vendorOrderID, sourceNode.Name, destNode.Name)

	return vendorOrderID, nil
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
	d.unclaimOrderInstances(order.ID)

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

	d.db.UpdateOrderDeliveryNode(order.ID, p.NewDeliveryNode)
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
		StationID:   stationID,
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
	d.unclaimOrderInstances(order.ID)
	d.emitter.EmitOrderFailed(order.ID, order.EdgeUUID, stationID, errorCode, detail)
	d.sendError(env, order.EdgeUUID, errorCode, detail)
}

func (d *Dispatcher) unclaimOrderInstances(orderID int64) {
	d.db.UnclaimOrderInstances(orderID)
}

// LaneLock returns the dispatcher's lane lock for external use.
func (d *Dispatcher) LaneLock() *LaneLock { return d.laneLock }
