package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

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
	payloadCode := p.PayloadCode
	d.dbg("order request: station=%s uuid=%s type=%s payload=%s delivery=%s pickup=%s",
		stationID, p.OrderUUID, p.OrderType, payloadCode, p.DeliveryNode, p.PickupNode)

	// Create order record
	payloadDesc := p.PayloadDesc
	if p.RetrieveEmpty && p.OrderType == OrderTypeRetrieve {
		payloadDesc = "retrieve_empty"
	}
	order := &store.Order{
		EdgeUUID:     p.OrderUUID,
		StationID:    stationID,
		OrderType:    p.OrderType,
		Status:       StatusPending,
		Quantity:     p.Quantity,
		PickupNode:   p.PickupNode,
		DeliveryNode: p.DeliveryNode,
		Priority:     p.Priority,
		PayloadDesc:  payloadDesc,
	}

	// Resolve payload template (optional — manual orders may not specify one)
	if payloadCode != "" {
		_, err := d.db.GetPayloadByCode(payloadCode)
		if err != nil {
			log.Printf("dispatch: payload %q not found: %v", payloadCode, err)
			d.dbg("error: payload %q not found: %v", payloadCode, err)
			d.sendError(env, p.OrderUUID, "payload_error", fmt.Sprintf("payload %q not found", payloadCode))
			return
		}
	}

	// Validate destination node exists; resolve synthetic nodes
	if p.DeliveryNode != "" {
		destNode, err := d.db.GetNodeByDotName(p.DeliveryNode)
		if err != nil {
			log.Printf("dispatch: delivery node %q not found: %v", p.DeliveryNode, err)
			d.dbg("error: delivery node %q not found: %v", p.DeliveryNode, err)
			d.sendError(env, p.OrderUUID, "invalid_node", fmt.Sprintf("delivery node %q not found", p.DeliveryNode))
			return
		}
		if destNode.IsSynthetic && d.resolver != nil {
			// Delivery always needs store resolution (find empty slot),
			// regardless of order type.
			result, err := d.resolver.Resolve(destNode, OrderTypeStore, payloadCode, nil)
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
	if err := d.db.UpdateOrderStatus(order.ID, StatusPending, "order received"); err != nil {
		log.Printf("dispatch: update order %d status to pending: %v", order.ID, err)
	}

	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, p.OrderType, payloadCode, p.DeliveryNode)

	switch p.OrderType {
	case OrderTypeRetrieve:
		d.handleRetrieve(order, env, payloadCode)
	case OrderTypeMove:
		d.handleMove(order, env, payloadCode)
	case OrderTypeStore:
		d.handleStore(order, env, payloadCode)
	default:
		log.Printf("dispatch: unknown order type %q", p.OrderType)
		d.failOrder(order, env, "unknown_type", fmt.Sprintf("unknown order type: %s", p.OrderType))
	}
}

func (d *Dispatcher) handleRetrieve(order *store.Order, env *protocol.Envelope, payloadCode string) {
	if err := d.db.UpdateOrderStatus(order.ID, StatusSourcing, "finding source"); err != nil {
		log.Printf("dispatch: update order %d status to sourcing: %v", order.ID, err)
	}

	// Empty bin retrieval — find an empty compatible bin instead of a loaded bin
	if order.PayloadDesc == "retrieve_empty" {
		d.handleRetrieveEmpty(order, env, payloadCode)
		return
	}

	var source *store.Bin
	var sourceNode *store.Node

	// Try group-aware resolution if a pickup node is specified and is an NGRP
	if order.PickupNode != "" && d.resolver != nil {
		pickupNode, err := d.db.GetNodeByDotName(order.PickupNode)
		if err == nil && pickupNode.IsSynthetic && pickupNode.NodeTypeCode == "NGRP" {
			result, err := d.resolver.Resolve(pickupNode, OrderTypeRetrieve, payloadCode, nil)
			if err != nil {
				// Check if buried — trigger reshuffle
				var buriedErr *BuriedError
				if errors.As(err, &buriedErr) {
					d.dbg("retrieve: bin %d buried in lane %d, planning reshuffle", buriedErr.Bin.ID, buriedErr.LaneID)
					d.handleBuriedReshuffle(order, env, buriedErr)
					return
				}
				d.dbg("retrieve: node group resolution failed for %s: %v", order.PickupNode, err)
				d.failOrder(order, env, "no_source", fmt.Sprintf("no source in node group %s: %v", order.PickupNode, err))
				return
			}
			source = result.Bin
			sourceNode, _ = d.db.GetNode(*source.NodeID)
		}
	}

	// Fallback: global FIFO source selection
	if source == nil {
		var err error
		source, err = d.db.FindSourceBinFIFO(payloadCode)
		if err != nil {
			d.dbg("retrieve: no source bin for payload %s", payloadCode)
			d.failOrder(order, env, "no_source", fmt.Sprintf("no source bin found for payload %s", payloadCode))
			return
		}
		sourceNode, err = d.db.GetNode(*source.NodeID)
		if err != nil {
			d.failOrder(order, env, "node_error", err.Error())
			return
		}
	}

	d.dbg("retrieve: FIFO source bin=%d payload=%s node=%s", source.ID, payloadCode, sourceNode.Name)

	// Claim the bin
	if err := d.db.ClaimBin(source.ID, order.ID); err != nil {
		d.failOrder(order, env, "claim_failed", err.Error())
		return
	}
	order.BinID = &source.ID
	if err := d.db.UpdateOrderBinID(order.ID, source.ID); err != nil {
		log.Printf("dispatch: update order %d bin_id: %v", order.ID, err)
	}

	order.PickupNode = sourceNode.Name
	if err := d.db.UpdateOrderPickupNode(order.ID, sourceNode.Name); err != nil {
		log.Printf("dispatch: update order %d pickup_node: %v", order.ID, err)
	}

	destNode, err := d.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		d.failOrder(order, env, "node_error", err.Error())
		return
	}

	d.dispatchToFleet(order, env, sourceNode, destNode)
}

// handleRetrieveEmpty finds an empty compatible bin and dispatches it to the requesting station.
func (d *Dispatcher) handleRetrieveEmpty(order *store.Order, env *protocol.Envelope, payloadCode string) {
	// Determine preferred zone from the destination node
	var preferZone string
	if order.DeliveryNode != "" {
		if destNode, err := d.db.GetNodeByDotName(order.DeliveryNode); err == nil {
			preferZone = destNode.Zone
		}
	}

	bin, err := d.db.FindEmptyCompatibleBin(payloadCode, preferZone)
	if err != nil {
		d.dbg("retrieve_empty: no empty bin for payload %s", payloadCode)
		d.failOrder(order, env, "no_empty_bin", fmt.Sprintf("no empty compatible bin for payload %s", payloadCode))
		return
	}

	d.dbg("retrieve_empty: found bin=%d label=%s at node=%s", bin.ID, bin.Label, bin.NodeName)

	if err := d.db.ClaimBin(bin.ID, order.ID); err != nil {
		d.failOrder(order, env, "claim_failed", err.Error())
		return
	}
	order.BinID = &bin.ID
	if err := d.db.UpdateOrderBinID(order.ID, bin.ID); err != nil {
		log.Printf("dispatch: update order %d bin_id: %v", order.ID, err)
	}

	sourceNode, err := d.db.GetNode(*bin.NodeID)
	if err != nil {
		d.failOrder(order, env, "node_error", err.Error())
		return
	}
	order.PickupNode = sourceNode.Name
	if err := d.db.UpdateOrderPickupNode(order.ID, sourceNode.Name); err != nil {
		log.Printf("dispatch: update order %d pickup_node: %v", order.ID, err)
	}

	destNode, err := d.db.GetNodeByDotName(order.DeliveryNode)
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

	plan, err := PlanReshuffle(d.db, buried.Bin, buried.Slot, lane, *lane.ParentID)
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

	if err := d.db.UpdateOrderStatus(order.ID, StatusReshuffling, fmt.Sprintf("reshuffling lane — %d steps", len(plan.Steps))); err != nil {
		log.Printf("dispatch: update order %d status to reshuffling: %v", order.ID, err)
	}
	d.dbg("retrieve: compound reshuffle created for order %d: %d steps", order.ID, len(plan.Steps))

	// Advance to first step
	d.AdvanceCompoundOrder(order.ID)
}

func (d *Dispatcher) handleMove(order *store.Order, env *protocol.Envelope, payloadCode string) {
	if err := d.db.UpdateOrderStatus(order.ID, StatusSourcing, "validating move"); err != nil {
		log.Printf("dispatch: update order %d status to sourcing: %v", order.ID, err)
	}

	if order.PickupNode == "" {
		d.failOrder(order, env, "missing_pickup", "move order requires pickup_node")
		return
	}

	pickupNode, err := d.db.GetNodeByDotName(order.PickupNode)
	if err != nil {
		d.failOrder(order, env, "invalid_node", fmt.Sprintf("pickup node %q not found", order.PickupNode))
		return
	}

	// Find a bin at the pickup node to claim
	bins, _ := d.db.ListBinsByNode(pickupNode.ID)
	binClaimed := false
	for _, bin := range bins {
		if bin.ClaimedBy != nil {
			continue
		}
		// If a payload code is specified, validate the bin matches
		if payloadCode != "" && bin.PayloadCode != payloadCode {
			continue
		}
		if err := d.db.ClaimBin(bin.ID, order.ID); err == nil {
			order.BinID = &bin.ID
			if err := d.db.UpdateOrderBinID(order.ID, bin.ID); err != nil {
				log.Printf("dispatch: update order %d bin_id: %v", order.ID, err)
			}
			d.dbg("move: claimed bin=%d at %s", bin.ID, order.PickupNode)
			binClaimed = true
			break
		}
	}
	if !binClaimed && payloadCode != "" {
		d.dbg("move: no unclaimed bin with %s at %s", payloadCode, order.PickupNode)
		d.failOrder(order, env, "no_payload", fmt.Sprintf("no unclaimed %s bin at %s", payloadCode, order.PickupNode))
		return
	}

	if err := d.db.UpdateOrderPickupNode(order.ID, pickupNode.Name); err != nil {
		log.Printf("dispatch: update order %d pickup_node: %v", order.ID, err)
	}

	destNode, err := d.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		d.failOrder(order, env, "node_error", err.Error())
		return
	}

	d.dispatchToFleet(order, env, pickupNode, destNode)
}

func (d *Dispatcher) handleStore(order *store.Order, env *protocol.Envelope, payloadCode string) {
	if err := d.db.UpdateOrderStatus(order.ID, StatusSourcing, "finding storage destination"); err != nil {
		log.Printf("dispatch: update order %d status to sourcing: %v", order.ID, err)
	}

	// Capture the original delivery node before overwriting — it may be the pickup source
	originalDeliveryNode := order.DeliveryNode

	destNode, err := d.db.FindStorageDestination(payloadCode)
	if err != nil {
		d.dbg("store: no available storage node")
		d.failOrder(order, env, "no_storage", "no available storage node found")
		return
	}
	d.dbg("store: selected destination=%s for order %d", destNode.Name, order.ID)
	order.DeliveryNode = destNode.Name
	if err := d.db.UpdateOrderDeliveryNode(order.ID, destNode.Name); err != nil {
		log.Printf("dispatch: update order %d delivery_node: %v", order.ID, err)
	}

	// Pickup is the requesting line
	var pickupNode *store.Node
	if order.PickupNode != "" {
		pickupNode, err = d.db.GetNodeByDotName(order.PickupNode)
		if err != nil {
			d.failOrder(order, env, "invalid_node", fmt.Sprintf("pickup node %q not found", order.PickupNode))
			return
		}
	} else if originalDeliveryNode != "" {
		// Use original delivery_node as source for store ops (line-side -> storage)
		pickupNode, err = d.db.GetNodeByDotName(originalDeliveryNode)
		if err != nil {
			d.failOrder(order, env, "invalid_node", fmt.Sprintf("node %q not found", originalDeliveryNode))
			return
		}
	}

	if pickupNode == nil {
		d.failOrder(order, env, "missing_pickup", "store order requires a pickup location")
		return
	}

	// Claim bin at pickup node if not already set
	if order.BinID == nil {
		bins, _ := d.db.ListBinsByNode(pickupNode.ID)
		for _, bin := range bins {
			if bin.ClaimedBy == nil {
				if err := d.db.ClaimBin(bin.ID, order.ID); err == nil {
					order.BinID = &bin.ID
					if err := d.db.UpdateOrderBinID(order.ID, bin.ID); err != nil {
						log.Printf("dispatch: update order %d bin_id: %v", order.ID, err)
					}
					d.dbg("store: claimed bin=%d at %s", bin.ID, pickupNode.Name)
					break
				}
			}
		}
	}

	if err := d.db.UpdateOrderPickupNode(order.ID, pickupNode.Name); err != nil {
		log.Printf("dispatch: update order %d pickup_node: %v", order.ID, err)
	}

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

	if err := d.db.UpdateOrderVendor(order.ID, vendorOrderID, "CREATED", ""); err != nil {
		log.Printf("dispatch: update order %d vendor: %v", order.ID, err)
	}
	if err := d.db.UpdateOrderStatus(order.ID, StatusDispatched, fmt.Sprintf("vendor order %s created", vendorOrderID)); err != nil {
		log.Printf("dispatch: update order %d status to dispatched: %v", order.ID, err)
	}

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
		if dbErr := d.db.UpdateOrderStatus(order.ID, StatusFailed, err.Error()); dbErr != nil {
			log.Printf("dispatch: update order %d status to failed: %v", order.ID, dbErr)
		}
		return "", err
	}

	if err := d.db.UpdateOrderVendor(order.ID, vendorOrderID, "CREATED", ""); err != nil {
		log.Printf("dispatch: update order %d vendor: %v", order.ID, err)
	}
	if err := d.db.UpdateOrderStatus(order.ID, StatusDispatched, fmt.Sprintf("vendor order %s created", vendorOrderID)); err != nil {
		log.Printf("dispatch: update order %d status to dispatched: %v", order.ID, err)
	}
	d.emitter.EmitOrderDispatched(order.ID, vendorOrderID, sourceNode.Name, destNode.Name)

	return vendorOrderID, nil
}

// checkOwnership verifies the envelope sender owns the order.
// Core-role senders (e.g. UI-initiated actions) are always allowed.
func (d *Dispatcher) checkOwnership(env *protocol.Envelope, order *store.Order) bool {
	if env.Src.Role == protocol.RoleCore {
		return true
	}
	return env.Src.Station == order.StationID
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

	if !d.checkOwnership(env, order) {
		log.Printf("dispatch: cancel rejected — station %s does not own order %s (owner: %s)", stationID, p.OrderUUID, order.StationID)
		d.sendError(env, p.OrderUUID, "forbidden", "station does not own this order")
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
	d.unclaimOrder(order.ID)

	if err := d.db.UpdateOrderStatus(order.ID, StatusCancelled, p.Reason); err != nil {
		log.Printf("dispatch: update order %d status to cancelled: %v", order.ID, err)
	}

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
	if err := d.db.EnqueueOutbox(d.dispatchTopic, data, "order.cancelled", stationID); err != nil {
		log.Printf("dispatch: enqueue cancelled reply for order %s: %v", p.OrderUUID, err)
	}
}

// HandleOrderReceipt processes a delivery confirmation from ShinGo Edge.
func (d *Dispatcher) HandleOrderReceipt(env *protocol.Envelope, p *protocol.OrderReceipt) {
	stationID := env.Src.Station
	d.dbg("delivery receipt: station=%s uuid=%s type=%s count=%d", stationID, p.OrderUUID, p.ReceiptType, p.FinalCount)

	order, err := d.db.GetOrderByUUID(p.OrderUUID)
	if err != nil {
		log.Printf("dispatch: delivery receipt order %s not found: %v", p.OrderUUID, err)
		return
	}

	if !d.checkOwnership(env, order) {
		log.Printf("dispatch: receipt rejected — station %s does not own order %s (owner: %s)", stationID, p.OrderUUID, order.StationID)
		return
	}

	if err := d.db.UpdateOrderStatus(order.ID, StatusConfirmed, fmt.Sprintf("receipt: %s, count: %d", p.ReceiptType, p.FinalCount)); err != nil {
		log.Printf("dispatch: update order %d status to confirmed: %v", order.ID, err)
	}

	// Transition confirmed -> completed
	if err := d.db.CompleteOrder(order.ID); err != nil {
		log.Printf("dispatch: complete order %d: %v", order.ID, err)
	}
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

	if !d.checkOwnership(env, order) {
		log.Printf("dispatch: redirect rejected — station %s does not own order %s (owner: %s)", env.Src.Station, p.OrderUUID, order.StationID)
		d.sendError(env, p.OrderUUID, "forbidden", "station does not own this order")
		return
	}

	// Cancel existing vendor order
	if order.VendorOrderID != "" {
		if err := d.backend.CancelOrder(order.VendorOrderID); err != nil {
			log.Printf("dispatch: cancel for redirect %s: %v", order.VendorOrderID, err)
		}
	}

	// Update destination
	newDest, err := d.db.GetNodeByDotName(p.NewDeliveryNode)
	if err != nil {
		log.Printf("dispatch: redirect dest %q not found: %v", p.NewDeliveryNode, err)
		d.sendError(env, p.OrderUUID, "invalid_node", fmt.Sprintf("redirect destination %q not found", p.NewDeliveryNode))
		return
	}

	if err := d.db.UpdateOrderDeliveryNode(order.ID, p.NewDeliveryNode); err != nil {
		log.Printf("dispatch: update order %d delivery_node: %v", order.ID, err)
	}
	order.DeliveryNode = p.NewDeliveryNode

	// Get source node for re-dispatch
	if order.PickupNode == "" {
		d.sendError(env, p.OrderUUID, "redirect_failed", "no source node for redirect")
		return
	}
	sourceNode, err := d.db.GetNodeByDotName(order.PickupNode)
	if err != nil {
		d.sendError(env, p.OrderUUID, "redirect_failed", err.Error())
		return
	}

	if err := d.db.UpdateOrderStatus(order.ID, StatusSourcing, fmt.Sprintf("redirecting to %s", p.NewDeliveryNode)); err != nil {
		log.Printf("dispatch: update order %d status to sourcing: %v", order.ID, err)
	}
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
	if err := d.db.UpdateOrderStatus(order.ID, StatusPending, "store order received"); err != nil {
		log.Printf("dispatch: update order %d status to pending: %v", order.ID, err)
	}

	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, p.OrderType, "", p.PickupNode)

	d.handleStore(order, env, "")
}

// HandleOrderIngest processes an ingest request: sets manifest on a bin and dispatches storage.
func (d *Dispatcher) HandleOrderIngest(env *protocol.Envelope, p *protocol.OrderIngestRequest) {
	stationID := env.Src.Station
	payloadCode := p.PayloadCode
	d.dbg("ingest: station=%s uuid=%s payload=%s bin=%s pickup=%s", stationID, p.OrderUUID, payloadCode, p.BinLabel, p.PickupNode)

	// Resolve payload template
	tmpl, err := d.db.GetPayloadByCode(payloadCode)
	if err != nil {
		d.sendError(env, p.OrderUUID, "payload_error", fmt.Sprintf("payload %q not found", payloadCode))
		return
	}

	// Find bin by label
	bin, err := d.db.GetBinByLabel(p.BinLabel)
	if err != nil {
		d.sendError(env, p.OrderUUID, "bin_error", fmt.Sprintf("bin %q not found", p.BinLabel))
		return
	}

	// Set manifest on the bin
	if len(p.Manifest) > 0 {
		// Build manifest JSON from provided items
		manifest := store.BinManifest{Items: make([]store.ManifestEntry, len(p.Manifest))}
		for i, item := range p.Manifest {
			manifest.Items[i] = store.ManifestEntry{
				CatID:    item.PartNumber,
				Quantity: item.Quantity,
			}
		}
		manifestJSON, _ := json.Marshal(manifest)
		if err := d.db.SetBinManifest(bin.ID, string(manifestJSON), payloadCode, tmpl.UOPCapacity); err != nil {
			d.sendError(env, p.OrderUUID, "internal_error", err.Error())
			return
		}
	} else {
		// Use default manifest from payload template
		if err := d.db.SetBinManifestFromTemplate(bin.ID, payloadCode, 0); err != nil {
			d.sendError(env, p.OrderUUID, "internal_error", err.Error())
			return
		}
	}

	// Confirm manifest and set loaded timestamp
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if err := d.db.ConfirmBinManifest(bin.ID); err != nil {
		log.Printf("dispatch: confirm bin %d manifest: %v", bin.ID, err)
	}

	d.dbg("ingest: set manifest on bin=%d, payload=%s, loaded_at=%s", bin.ID, payloadCode, now)

	// Create store order
	order := &store.Order{
		EdgeUUID:    p.OrderUUID,
		StationID:   stationID,
		OrderType:   OrderTypeStore,
		Status:      StatusPending,
		Quantity:    p.Quantity,
		PickupNode:  p.PickupNode,
		PayloadDesc: fmt.Sprintf("ingest %s bin %s", payloadCode, p.BinLabel),
		BinID:       &bin.ID,
	}

	if err := d.db.CreateOrder(order); err != nil {
		d.sendError(env, p.OrderUUID, "internal_error", err.Error())
		return
	}
	if err := d.db.UpdateOrderStatus(order.ID, StatusPending, "ingest order received"); err != nil {
		log.Printf("dispatch: update order %d status to pending: %v", order.ID, err)
	}

	// Claim the bin
	if err := d.db.ClaimBin(bin.ID, order.ID); err != nil {
		log.Printf("dispatch: claim bin %d for order %d: %v", bin.ID, order.ID, err)
	}

	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, OrderTypeStore, payloadCode, "")

	// Route to storage
	d.handleStore(order, env, payloadCode)
}

func (d *Dispatcher) failOrder(order *store.Order, env *protocol.Envelope, errorCode, detail string) {
	stationID := env.Src.Station
	if err := d.db.UpdateOrderStatus(order.ID, StatusFailed, detail); err != nil {
		log.Printf("dispatch: update order %d status to failed: %v", order.ID, err)
	}
	d.unclaimOrder(order.ID)
	d.emitter.EmitOrderFailed(order.ID, order.EdgeUUID, stationID, errorCode, detail)
	d.sendError(env, order.EdgeUUID, errorCode, detail)
}

func (d *Dispatcher) unclaimOrder(orderID int64) {
	d.db.UnclaimOrderBins(orderID)
}

// LaneLock returns the dispatcher's lane lock for external use.
func (d *Dispatcher) LaneLock() *LaneLock { return d.laneLock }
