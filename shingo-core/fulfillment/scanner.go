package fulfillment

import (
	"errors"
	"log"
	"sync"
	"time"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// Scanner monitors queued orders and fulfills them when matching
// inventory becomes available. Runs event-driven with a periodic
// safety sweep.
//
// Construct via NewScanner. The zero value is not usable.
//
// dispatcher and resolver are narrow consumer-side interfaces
// (see dispatcher.go). The concrete types *dispatch.Dispatcher and
// *dispatch.DefaultResolver satisfy them structurally, so the
// engine wires them in unchanged. Holding interfaces here is what
// lets scanner_test.go stub a one-method fake dispatcher.
type Scanner struct {
	db         Store
	dispatcher Dispatcher
	lifecycle  Lifecycle
	resolver   Resolver
	sendToEdge func(msgType, stationID string, payload any) error
	// failFn fails an order in the DB AND emits EventOrderFailed so the
	// standard handler chain (audit, return order, edge notification) fires.
	// Wired at construction to engine.failOrderAndEmit. Without this, the
	// scanner's structural-error path silently terminates orders with no
	// audit trail, no Edge notification, and no bin recovery.
	failFn   func(orderID int64, code, detail string)
	logFn    func(string, ...any)
	debugLog func(string, ...any)

	scanMu    sync.Mutex // serializes scan() calls
	triggerMu sync.Mutex
	pending   bool // coalesce triggers during a scan
	stopChan  chan struct{}
}

// NewScanner constructs a Scanner wired to the provided dependencies.
// See package doc for the role of each argument.
//
// dispatcher and resolver are accepted as narrow interfaces. Callers
// (engine) continue to pass the concrete *dispatch.Dispatcher and
// *dispatch.DefaultResolver — Go's structural typing handles the rest.
func NewScanner(
	db Store,
	dispatcher Dispatcher,
	lifecycle Lifecycle,
	resolver Resolver,
	sendFn func(string, string, any) error,
	failFn func(orderID int64, code, detail string),
	logFn func(string, ...any),
	debugLog func(string, ...any),
) *Scanner {
	return &Scanner{
		db:         db,
		dispatcher: dispatcher,
		lifecycle:  lifecycle,
		resolver:   resolver,
		sendToEdge: sendFn,
		failFn:     failFn,
		logFn:      logFn,
		debugLog:   debugLog,
		stopChan:   make(chan struct{}),
	}
}

// Trigger requests a scan. If a scan is already running, the request
// is coalesced — the scanner will re-run after the current scan finishes.
func (s *Scanner) Trigger() {
	s.triggerMu.Lock()
	s.pending = true
	s.triggerMu.Unlock()
}

// RunOnce executes a single scan pass. Only one scan runs at a time.
func (s *Scanner) RunOnce() int {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	s.triggerMu.Lock()
	s.pending = false
	s.triggerMu.Unlock()

	fulfilled := s.scan()

	// If events arrived during the scan, run again
	s.triggerMu.Lock()
	again := s.pending
	s.pending = false
	s.triggerMu.Unlock()
	if again {
		fulfilled += s.scan()
	}
	return fulfilled
}

// StartPeriodicSweep runs the scanner every interval as a safety net.
func (s *Scanner) StartPeriodicSweep(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopChan:
				return
			case <-ticker.C:
				s.RunOnce()
			}
		}
	}()
}

// Stop halts the periodic sweep.
func (s *Scanner) Stop() {
	close(s.stopChan)
}

func (s *Scanner) scan() int {
	orders, err := s.db.ListQueuedOrders()
	if err != nil {
		log.Printf("fulfillment: list queued orders: %v", err)
		return 0
	}
	if len(orders) == 0 {
		return 0
	}

	if s.debugLog != nil {
		s.debugLog("fulfillment: scanning %d queued orders", len(orders))
	}

	fulfilled := 0
	for _, order := range orders {
		if s.tryFulfill(order) {
			fulfilled++
		}
	}
	if fulfilled > 0 {
		s.logFn("fulfillment: fulfilled %d queued orders", fulfilled)
	}
	return fulfilled
}

func (s *Scanner) tryFulfill(order *orders.Order) bool {
	// Re-check status (may have been cancelled between listing and processing)
	current, err := s.db.GetOrder(order.ID)
	if err != nil || current.Status != protocol.StatusQueued {
		return false
	}

	// Use the fresh copy for all subsequent operations
	order = current

	// Check delivery node doesn't already have an in-flight delivery
	if order.DeliveryNode != "" {
		count, err := s.db.CountInFlightOrdersByDeliveryNode(order.DeliveryNode)
		if err == nil && count > 0 {
			return false
		}
	}

	// Check delivery node isn't already occupied by a bin. Without this,
	// a race exists: order A confirmed → operator unloads → scanner sees
	// 0 in-flight → dispatches order B → robot arrives while the outbound
	// bin from order A is still physically on the node.
	if order.DeliveryNode != "" {
		if destNode, err := s.db.GetNodeByDotName(order.DeliveryNode); err == nil {
			if count, err := s.db.CountBinsByNode(destNode.ID); err == nil && count > 0 {
				return false
			}
		}
	}

	payloadCode := order.PayloadCode
	if payloadCode == "" {
		return false
	}

	// Find a matching bin based on order type
	var bin *bins.Bin
	var sourceNode *nodes.Node

	if order.PayloadDesc == "retrieve_empty" {
		// Empty bin retrieval
		var preferZone string
		if order.DeliveryNode != "" {
			if destNode, err := s.db.GetNodeByDotName(order.DeliveryNode); err == nil {
				preferZone = destNode.Zone
			}
		}
		bin, err = s.db.FindEmptyCompatibleBin(payloadCode, preferZone)
		if err != nil {
			return false // still no empties
		}
	} else {
		// Normal retrieve — find a source bin with this payload
		if order.SourceNode != "" {
			// Try NGRP resolution first
			sourceNode, pErr := s.db.GetNodeByDotName(order.SourceNode)
			if pErr == nil && sourceNode.IsSynthetic && sourceNode.NodeTypeCode == "NGRP" && s.resolver != nil {
				result, rErr := s.resolver.Resolve(sourceNode, dispatch.OrderTypeRetrieve, payloadCode, nil)
				if rErr != nil {
					var structErr *dispatch.StructuralError
					if errors.As(rErr, &structErr) {
						// Use failFn so the standard EventOrderFailed handler
						// chain fires (audit, maybeCreateReturnOrder, edge
						// notification). Production wires failFn to
						// engine.failOrderAndEmit which routes through
						// lifecycle.Fail. The previous code had a
						// db.FailOrderAtomic fallback for the failFn==nil
						// case; that fallback bypassed the state machine
						// and masked construction bugs. If failFn is nil
						// here, log loudly and return — a nil failFn in
						// production is a wiring mistake the operator
						// should see.
						if s.failFn != nil {
							s.failFn(order.ID, "structural", structErr.Error())
						} else {
							s.logFn("fulfillment: order %d structural error but failFn not wired — order left in queued state, fix scanner construction: %v",
								order.ID, structErr)
						}
						s.logFn("fulfillment: order %d terminated — structural: %s",
							order.ID, structErr.Error())
						return false
					}
					// Transient: fall through to FindSourceBinFIFO
				} else {
					bin = result.Bin
				}
			}
		}
		if bin == nil {
			bin, err = s.db.FindSourceBinFIFO(payloadCode)
			if err != nil {
				return false // still no source
			}
		}
	}

	if bin == nil {
		return false
	}

	// Claim the bin
	if err := s.db.ClaimBin(bin.ID, order.ID); err != nil {
		if s.debugLog != nil {
			s.debugLog("fulfillment: claim bin %d for order %d failed: %v", bin.ID, order.ID, err)
		}
		return false
	}

	// Resolve source node
	sourceNode, err = s.db.GetNode(*bin.NodeID)
	if err != nil {
		s.db.UnclaimOrderBins(order.ID)
		return false
	}

	// Error handling policy: log and continue. Do not add early returns without understanding the caller contract. See 2567plandiscussion.md.
	// Update order with bin and source
	if err := s.db.UpdateOrderBinID(order.ID, bin.ID); err != nil {
		s.logFn("fulfillment: update bin_id for order %d: %v", order.ID, err)
	}
	if err := s.db.UpdateOrderSourceNode(order.ID, sourceNode.Name); err != nil {
		s.logFn("fulfillment: update source_node for order %d: %v", order.ID, err)
	}
	if err := s.lifecycle.MoveToSourcing(order, "fulfillment", "bin found, dispatching"); err != nil {
		s.logFn("fulfillment: order %d → sourcing: %v", order.ID, err)
	}

	// Resolve destination
	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		s.logFn("fulfillment: dest node %q not found for order %d: %v", order.DeliveryNode, order.ID, err)
		s.db.UnclaimOrderBins(order.ID)
		if err := s.lifecycle.Queue(order, "fulfillment", "awaiting inventory"); err != nil {
			s.logFn("fulfillment: order %d → queued: %v", order.ID, err)
		}
		return false
	}

	// Dispatch to fleet — use DispatchDirect which handles fleet creation.
	// On failure, DispatchDirect sets status to failed. We override back to queued
	// since this is a transient fleet issue, not a permanent failure.
	vendorOrderID, err := s.dispatcher.DispatchDirect(order, sourceNode, destNode)
	if err != nil {
		s.logFn("fulfillment: fleet dispatch failed for order %d, re-queuing: %v", order.ID, err)
		s.db.UnclaimOrderBins(order.ID)
		if err := s.lifecycle.Queue(order, "fulfillment", "fleet unavailable, re-queued"); err != nil {
			s.logFn("fulfillment: order %d → queued: %v", order.ID, err)
		}
		return false
	}

	s.logFn("fulfillment: order %d fulfilled — bin %d (%s -> %s) vendor=%s",
		order.ID, bin.ID, sourceNode.Name, destNode.Name, vendorOrderID)

	// Notify Edge: ack + waybill
	if order.StationID != "" {
		if err := s.sendToEdge(protocol.TypeOrderAck, order.StationID, &protocol.OrderAck{
			OrderUUID:     order.EdgeUUID,
			ShingoOrderID: order.ID,
			SourceNode:    sourceNode.Name,
		}); err != nil {
			s.logFn("fulfillment: ack for order %d: %v", order.ID, err)
		}
		if err := s.sendToEdge(protocol.TypeOrderWaybill, order.StationID, &protocol.OrderWaybill{
			OrderUUID: order.EdgeUUID,
			WaybillID: vendorOrderID,
		}); err != nil {
			s.logFn("fulfillment: waybill for order %d: %v", order.ID, err)
		}
	}

	return true
}
