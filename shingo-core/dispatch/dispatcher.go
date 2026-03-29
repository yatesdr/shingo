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
	resolver      NodeResolver
	laneLock      *LaneLock
	stationID     string
	dispatchTopic string
	lifecycle     *LifecycleService
	replies       *ReplySender
	planner       *PlanningService
	DebugLog      func(string, ...any)
}

func NewDispatcher(db *store.DB, backend fleet.Backend, emitter Emitter, stationID, dispatchTopic string, resolver NodeResolver) *Dispatcher {
	d := &Dispatcher{
		db:            db,
		backend:       backend,
		emitter:       emitter,
		resolver:      resolver,
		laneLock:      NewLaneLock(),
		stationID:     stationID,
		dispatchTopic: dispatchTopic,
	}
	d.lifecycle = newLifecycleService(db, backend, emitter, resolver, d.dbg)
	d.replies = newReplySender(db, dispatchTopic, stationID, d.dbg)
	d.planner = newPlanningService(db, resolver, d.laneLock, d.dbg, d.CreateCompoundOrder, d.AdvanceCompoundOrder)
	return d
}

func (d *Dispatcher) dbg(format string, args ...any) {
	if fn := d.DebugLog; fn != nil {
		fn(format, args...)
	}
}

func (d *Dispatcher) RegisterPlanner(orderType string, handler PlanningHandler) {
	d.planner.Register(orderType, handler)
}

// HandleOrderRequest processes a new order from ShinGo Edge.
func (d *Dispatcher) HandleOrderRequest(env *protocol.Envelope, p *protocol.OrderRequest) {
	stationID := env.Src.Station
	d.dbg("order request: station=%s uuid=%s type=%s payload=%s delivery=%s source=%s",
		stationID, p.OrderUUID, p.OrderType, p.PayloadCode, p.DeliveryNode, p.SourceNode)

	order, payloadCode, lifecycleErr := d.lifecycle.CreateInboundOrder(stationID, p)
	if lifecycleErr != nil {
		if lifecycleErr.Err != nil {
			log.Printf("dispatch: create inbound order %s: %v", p.OrderUUID, lifecycleErr.Err)
		}
		d.replies.SendError(env, p.OrderUUID, lifecycleErr.Code, lifecycleErr.Detail)
		return
	}

	result, planErr := d.planner.Plan(order, env, payloadCode)
	if planErr != nil {
		if planErr.Err != nil {
			log.Printf("dispatch: plan order %s (%s): %v", p.OrderUUID, p.OrderType, planErr.Err)
		} else {
			log.Printf("dispatch: plan order %s (%s): %s", p.OrderUUID, p.OrderType, planErr.Detail)
		}
		// claim_failed is transient: bins exist but were claimed by a concurrent
		// order in the TOCTOU gap between FindSourceBinFIFO and ClaimBin. Queue
		// the order so the fulfillment scanner retries when a bin frees up.
		if planErr.Code == "claim_failed" {
			d.dbg("dispatch: claim_failed for order %s — queuing for retry", p.OrderUUID)
			d.queueOrder(order, env, payloadCode)
			return
		}
		d.failOrder(order, env, planErr.Code, planErr.Detail)
		return
	}
	if result == nil || result.Handled {
		return
	}
	if result.Queued {
		d.queueOrder(order, env, payloadCode)
		return
	}
	d.dispatchToFleet(order, env, result.SourceNode, result.DestNode)
}

func (d *Dispatcher) queueOrder(order *store.Order, env *protocol.Envelope, payloadCode string) {
	if err := d.db.UpdateOrderStatus(order.ID, StatusQueued, "awaiting inventory"); err != nil {
		log.Printf("dispatch: queue order %d: %v", order.ID, err)
	}
	if payloadCode != "" && order.PayloadCode == "" {
		_ = d.db.UpdateOrderPayloadCode(order.ID, payloadCode)
	}
	d.dbg("queued: order=%d uuid=%s payload=%s delivery=%s", order.ID, order.EdgeUUID, payloadCode, order.DeliveryNode)
	d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, env.Src.Station, payloadCode)
	d.replies.SendUpdate(env, order.EdgeUUID, StatusQueued, "awaiting inventory")
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
		d.unclaimOrder(order.ID)
		d.emitter.EmitOrderFailed(order.ID, order.EdgeUUID, order.StationID, "fleet_failed", err.Error())
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
		d.replies.SendError(env, p.OrderUUID, "forbidden", "station does not own this order")
		return
	}
	if order.Status == StatusCancelled {
		d.dbg("cancel request: uuid=%s already cancelled", p.OrderUUID)
		return
	}

	d.lifecycle.CancelOrder(order, stationID, p.Reason)
	d.replies.SendCancelled(env, p.OrderUUID, p.Reason)
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
	if _, err := d.lifecycle.ConfirmReceipt(order, stationID, p.ReceiptType, p.FinalCount); err != nil {
		log.Printf("dispatch: complete order %d: %v", order.ID, err)
	}
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
		d.replies.SendError(env, p.OrderUUID, "forbidden", "station does not own this order")
		return
	}
	sourceNode, newDest, err := d.lifecycle.PrepareRedirect(order, p.NewDeliveryNode)
	if err != nil {
		if err.Error() == "no source node for redirect" {
			d.replies.SendError(env, p.OrderUUID, "redirect_failed", err.Error())
			return
		}
		if sourceNode == nil || newDest == nil {
			log.Printf("dispatch: redirect dest %q not found: %v", p.NewDeliveryNode, err)
			d.replies.SendError(env, p.OrderUUID, "invalid_node", fmt.Sprintf("redirect destination %q not found", p.NewDeliveryNode))
			return
		}
		if err != nil {
			d.replies.SendError(env, p.OrderUUID, "redirect_failed", err.Error())
			return
		}
	}
	if newDest == nil {
		log.Printf("dispatch: redirect dest %q not found: %v", p.NewDeliveryNode, err)
		d.replies.SendError(env, p.OrderUUID, "invalid_node", fmt.Sprintf("redirect destination %q not found", p.NewDeliveryNode))
		return
	}
	d.dispatchToFleet(order, env, sourceNode, newDest)
}

// HandleOrderStorageWaybill processes a storage waybill from ShinGo Edge.
func (d *Dispatcher) HandleOrderStorageWaybill(env *protocol.Envelope, p *protocol.OrderStorageWaybill) {
	stationID := env.Src.Station
	d.dbg("storage waybill: station=%s uuid=%s type=%s source=%s", stationID, p.OrderUUID, p.OrderType, p.SourceNode)

	order, lifecycleErr := d.lifecycle.CreateStorageWaybillOrder(stationID, p)
	if lifecycleErr != nil {
		log.Printf("dispatch: create store order: %v", lifecycleErr.Err)
		d.replies.SendError(env, p.OrderUUID, lifecycleErr.Code, lifecycleErr.Detail)
		return
	}
	result, planErr := d.planner.Plan(order, env, "")
	if planErr != nil {
		d.failOrder(order, env, planErr.Code, planErr.Detail)
		return
	}
	if result != nil && !result.Handled {
		d.dispatchToFleet(order, env, result.SourceNode, result.DestNode)
	}
}

// HandleOrderIngest processes an ingest request: sets manifest on a bin and dispatches storage.
func (d *Dispatcher) HandleOrderIngest(env *protocol.Envelope, p *protocol.OrderIngestRequest) {
	stationID := env.Src.Station
	payloadCode := p.PayloadCode
	d.dbg("ingest: station=%s uuid=%s payload=%s bin=%s source=%s", stationID, p.OrderUUID, payloadCode, p.BinLabel, p.SourceNode)

	order, payloadCode, lifecycleErr := d.lifecycle.CreateIngestStoreOrder(stationID, p)
	if lifecycleErr != nil {
		d.replies.SendError(env, p.OrderUUID, lifecycleErr.Code, lifecycleErr.Detail)
		return
	}

	result, planErr := d.planner.Plan(order, env, payloadCode)
	if planErr != nil {
		d.failOrder(order, env, planErr.Code, planErr.Detail)
		return
	}
	if result != nil && !result.Handled {
		d.dispatchToFleet(order, env, result.SourceNode, result.DestNode)
	}
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

// Lifecycle returns the dispatcher's lifecycle service for external use (e.g. auto-confirm).
func (d *Dispatcher) Lifecycle() *LifecycleService { return d.lifecycle }
