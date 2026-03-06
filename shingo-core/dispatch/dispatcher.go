package dispatch

import (
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
	blueprintCode := p.EffectiveBlueprintCode()
	d.dbg("order request: station=%s uuid=%s type=%s blueprint=%s delivery=%s pickup=%s",
		stationID, p.OrderUUID, p.OrderType, blueprintCode, p.DeliveryNode, p.PickupNode)

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

	// Resolve blueprint (optional — manual orders may not specify one)
	if blueprintCode != "" {
		bp, err := d.db.GetBlueprintByCode(blueprintCode)
		if err != nil {
			log.Printf("dispatch: blueprint %q not found: %v", blueprintCode, err)
			d.dbg("error: blueprint %q not found: %v", blueprintCode, err)
			d.sendError(env, p.OrderUUID, "blueprint_error", fmt.Sprintf("blueprint %q not found", blueprintCode))
			return
		}
		order.BlueprintID = &bp.ID
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
			result, err := d.resolver.Resolve(destNode, OrderTypeStore, order.BlueprintID, nil)
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

	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, p.OrderType, blueprintCode, p.DeliveryNode)

	switch p.OrderType {
	case OrderTypeRetrieve:
		d.handleRetrieve(order, env, blueprintCode)
	case OrderTypeMove:
		d.handleMove(order, env, blueprintCode)
	case OrderTypeStore:
		d.handleStore(order, env)
	default:
		log.Printf("dispatch: unknown order type %q", p.OrderType)
		d.failOrder(order, env, "unknown_type", fmt.Sprintf("unknown order type: %s", p.OrderType))
	}
}

func (d *Dispatcher) handleRetrieve(order *store.Order, env *protocol.Envelope, blueprintCode string) {
	d.db.UpdateOrderStatus(order.ID, StatusSourcing, "finding source")

	// Empty bin retrieval — find an empty compatible bin instead of a payload
	if order.PayloadDesc == "retrieve_empty" {
		d.handleRetrieveEmpty(order, env, blueprintCode)
		return
	}

	var source *store.Payload
	var sourceNode *store.Node

	// Try group-aware resolution if a pickup node is specified and is an NGRP
	if order.PickupNode != "" && d.resolver != nil {
		pickupNode, err := d.db.GetNodeByDotName(order.PickupNode)
		if err == nil && pickupNode.IsSynthetic && pickupNode.NodeTypeCode == "NGRP" {
			result, err := d.resolver.Resolve(pickupNode, OrderTypeRetrieve, order.BlueprintID, nil)
			if err != nil {
				// Check if buried — trigger reshuffle
				var buriedErr *BuriedError
				if errors.As(err, &buriedErr) {
					d.dbg("retrieve: payload %d buried in lane %d, planning reshuffle", buriedErr.Payload.ID, buriedErr.LaneID)
					d.handleBuriedReshuffle(order, env, buriedErr)
					return
				}
				d.dbg("retrieve: node group resolution failed for %s: %v", order.PickupNode, err)
				d.failOrder(order, env, "no_source", fmt.Sprintf("no source in node group %s: %v", order.PickupNode, err))
				return
			}
			source = result.Payload
			sourceNode, _ = d.db.GetNode(*source.NodeID)
		}
	}

	// Fallback: global FIFO source selection
	if source == nil {
		var err error
		source, err = d.db.FindSourcePayloadFIFO(blueprintCode)
		if err != nil {
			d.dbg("retrieve: no source payload for blueprint %s", blueprintCode)
			d.failOrder(order, env, "no_source", fmt.Sprintf("no source payload found for blueprint %s", blueprintCode))
			return
		}
		sourceNode, err = d.db.GetNode(*source.NodeID)
		if err != nil {
			d.failOrder(order, env, "node_error", err.Error())
			return
		}
	}

	d.dbg("retrieve: FIFO source payload=%d blueprint=%s node=%s", source.ID, blueprintCode, sourceNode.Name)

	// Claim the bin (bin-centric)
	if source.BinID != nil {
		if err := d.db.ClaimBin(*source.BinID, order.ID); err != nil {
			d.failOrder(order, env, "claim_failed", err.Error())
			return
		}
		order.BinID = source.BinID
		d.db.UpdateOrderBinID(order.ID, *source.BinID)
	}

	order.PickupNode = sourceNode.Name
	d.db.UpdateOrderPickupNode(order.ID, sourceNode.Name)

	destNode, err := d.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		d.failOrder(order, env, "node_error", err.Error())
		return
	}

	d.dispatchToFleet(order, env, sourceNode, destNode)
}

// handleRetrieveEmpty finds an empty compatible bin and dispatches it to the requesting station.
func (d *Dispatcher) handleRetrieveEmpty(order *store.Order, env *protocol.Envelope, blueprintCode string) {
	// Determine preferred zone from the destination node
	var preferZone string
	if order.DeliveryNode != "" {
		if destNode, err := d.db.GetNodeByDotName(order.DeliveryNode); err == nil {
			preferZone = destNode.Zone
		}
	}

	bin, err := d.db.FindEmptyCompatibleBin(blueprintCode, preferZone)
	if err != nil {
		d.dbg("retrieve_empty: no empty bin for blueprint %s", blueprintCode)
		d.failOrder(order, env, "no_empty_bin", fmt.Sprintf("no empty compatible bin for blueprint %s", blueprintCode))
		return
	}

	d.dbg("retrieve_empty: found bin=%d label=%s at node=%s", bin.ID, bin.Label, bin.NodeName)

	if err := d.db.ClaimBin(bin.ID, order.ID); err != nil {
		d.failOrder(order, env, "claim_failed", err.Error())
		return
	}
	order.BinID = &bin.ID
	d.db.UpdateOrderBinID(order.ID, bin.ID)

	sourceNode, err := d.db.GetNode(*bin.NodeID)
	if err != nil {
		d.failOrder(order, env, "node_error", err.Error())
		return
	}
	order.PickupNode = sourceNode.Name
	d.db.UpdateOrderPickupNode(order.ID, sourceNode.Name)

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

	plan, err := PlanReshuffle(d.db, buried.Payload, buried.Slot, lane, *lane.ParentID)
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

func (d *Dispatcher) handleMove(order *store.Order, env *protocol.Envelope, blueprintCode string) {
	d.db.UpdateOrderStatus(order.ID, StatusSourcing, "validating move")

	if order.PickupNode == "" {
		d.failOrder(order, env, "missing_pickup", "move order requires pickup_node")
		return
	}

	pickupNode, err := d.db.GetNodeByDotName(order.PickupNode)
	if err != nil {
		d.failOrder(order, env, "invalid_node", fmt.Sprintf("pickup node %q not found", order.PickupNode))
		return
	}

	// Find a bin at the pickup node to claim (bin-centric)
	bins, _ := d.db.ListBinsByNode(pickupNode.ID)
	binClaimed := false
	for _, bin := range bins {
		if bin.ClaimedBy != nil {
			continue
		}
		// If a blueprint is specified, validate via payload on the bin
		if blueprintCode != "" {
			payloads, _ := d.db.ListPayloadsByBin(bin.ID)
			match := false
			for _, pl := range payloads {
				if pl.BlueprintCode == blueprintCode {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		if err := d.db.ClaimBin(bin.ID, order.ID); err == nil {
			order.BinID = &bin.ID
			d.db.UpdateOrderBinID(order.ID, bin.ID)
			d.dbg("move: claimed bin=%d at %s", bin.ID, order.PickupNode)
			binClaimed = true
			break
		}
	}
	if !binClaimed && blueprintCode != "" {
		d.dbg("move: no unclaimed bin with %s at %s", blueprintCode, order.PickupNode)
		d.failOrder(order, env, "no_payload", fmt.Sprintf("no unclaimed %s bin at %s", blueprintCode, order.PickupNode))
		return
	}

	d.db.UpdateOrderPickupNode(order.ID, pickupNode.Name)

	destNode, err := d.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		d.failOrder(order, env, "node_error", err.Error())
		return
	}

	d.dispatchToFleet(order, env, pickupNode, destNode)
}

func (d *Dispatcher) handleStore(order *store.Order, env *protocol.Envelope) {
	d.db.UpdateOrderStatus(order.ID, StatusSourcing, "finding storage destination")

	var blueprintID int64
	if order.BlueprintID != nil {
		blueprintID = *order.BlueprintID
	}

	// Capture the original delivery node before overwriting — it may be the pickup source
	originalDeliveryNode := order.DeliveryNode

	destNode, err := d.db.FindStorageDestination(blueprintID)
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

	// Claim bin at pickup node if not already set (bin-centric)
	if order.BinID == nil {
		bins, _ := d.db.ListBinsByNode(pickupNode.ID)
		for _, bin := range bins {
			if bin.ClaimedBy == nil {
				if err := d.db.ClaimBin(bin.ID, order.ID); err == nil {
					order.BinID = &bin.ID
					d.db.UpdateOrderBinID(order.ID, bin.ID)
					d.dbg("store: claimed bin=%d at %s", bin.ID, pickupNode.Name)
					break
				}
			}
		}
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

	d.db.UpdateOrderStatus(order.ID, StatusConfirmed, fmt.Sprintf("receipt: %s, count: %d", p.ReceiptType, p.FinalCount))

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

	d.db.UpdateOrderDeliveryNode(order.ID, p.NewDeliveryNode)
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

// HandleOrderIngest processes an ingest request: creates a payload on a bin and dispatches storage.
func (d *Dispatcher) HandleOrderIngest(env *protocol.Envelope, p *protocol.OrderIngestRequest) {
	stationID := env.Src.Station
	d.dbg("ingest: station=%s uuid=%s blueprint=%s bin=%s pickup=%s", stationID, p.OrderUUID, p.BlueprintCode, p.BinLabel, p.PickupNode)

	// Resolve blueprint
	bp, err := d.db.GetBlueprintByCode(p.BlueprintCode)
	if err != nil {
		d.sendError(env, p.OrderUUID, "blueprint_error", fmt.Sprintf("blueprint %q not found", p.BlueprintCode))
		return
	}

	// Find bin by label
	bin, err := d.db.GetBinByLabel(p.BinLabel)
	if err != nil {
		d.sendError(env, p.OrderUUID, "bin_error", fmt.Sprintf("bin %q not found", p.BinLabel))
		return
	}

	// Create payload on the bin
	now := time.Now()
	payload := &store.Payload{
		BlueprintID:  bp.ID,
		BinID:        &bin.ID,
		Status:       "available",
		UOPRemaining: bp.UOPCapacity,
		LoadedAt:     &now,
	}

	if len(p.Manifest) > 0 {
		// Create payload with provided manifest
		if err := d.db.CreatePayload(payload); err != nil {
			d.sendError(env, p.OrderUUID, "internal_error", err.Error())
			return
		}
		for _, item := range p.Manifest {
			d.db.CreateManifestItem(&store.ManifestItem{
				PayloadID:  payload.ID,
				PartNumber: item.PartNumber,
				Quantity:   item.Quantity,
				Notes:      item.Description,
			})
		}
	} else {
		// Create payload with default manifest from blueprint
		if err := d.db.CreatePayloadWithManifest(payload); err != nil {
			d.sendError(env, p.OrderUUID, "internal_error", err.Error())
			return
		}
	}

	d.dbg("ingest: created payload=%d on bin=%d", payload.ID, bin.ID)

	// Create store order
	order := &store.Order{
		EdgeUUID:    p.OrderUUID,
		StationID:   stationID,
		OrderType:   OrderTypeStore,
		Status:      StatusPending,
		Quantity:    p.Quantity,
		PickupNode:  p.PickupNode,
		PayloadDesc: fmt.Sprintf("ingest %s bin %s", p.BlueprintCode, p.BinLabel),
		BlueprintID: &bp.ID,
		PayloadID:   &payload.ID,
		BinID:       &bin.ID,
	}

	if err := d.db.CreateOrder(order); err != nil {
		d.sendError(env, p.OrderUUID, "internal_error", err.Error())
		return
	}
	d.db.UpdateOrderStatus(order.ID, StatusPending, "ingest order received")

	// Claim the bin (bin-centric, no payload claim needed)
	d.db.ClaimBin(bin.ID, order.ID)

	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, OrderTypeStore, p.BlueprintCode, "")

	// Route to storage
	d.handleStore(order, env)
}

func (d *Dispatcher) failOrder(order *store.Order, env *protocol.Envelope, errorCode, detail string) {
	stationID := env.Src.Station
	d.db.UpdateOrderStatus(order.ID, StatusFailed, detail)
	d.unclaimOrder(order.ID)
	d.emitter.EmitOrderFailed(order.ID, order.EdgeUUID, stationID, errorCode, detail)
	d.sendError(env, order.EdgeUUID, errorCode, detail)
}

func (d *Dispatcher) unclaimOrder(orderID int64) {
	d.db.UnclaimOrderBins(orderID)
}

// LaneLock returns the dispatcher's lane lock for external use.
func (d *Dispatcher) LaneLock() *LaneLock { return d.laneLock }
