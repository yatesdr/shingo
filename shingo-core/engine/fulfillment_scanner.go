package engine

import (
	"log"
	"sync"
	"time"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/store"
)

// FulfillmentScanner monitors queued orders and fulfills them when
// matching inventory becomes available. Runs event-driven with a
// periodic safety sweep.
type FulfillmentScanner struct {
	db         *store.DB
	dispatcher *dispatch.Dispatcher
	resolver   *dispatch.DefaultResolver
	sendToEdge func(msgType, stationID string, payload any) error
	logFn      func(string, ...any)
	debugLog   func(string, ...any)

	scanMu   sync.Mutex // serializes scan() calls
	triggerMu sync.Mutex
	pending   bool // coalesce triggers during a scan
	stopChan  chan struct{}
}

func newFulfillmentScanner(
	db *store.DB,
	dispatcher *dispatch.Dispatcher,
	resolver *dispatch.DefaultResolver,
	sendFn func(string, string, any) error,
	logFn func(string, ...any),
	debugLog func(string, ...any),
) *FulfillmentScanner {
	return &FulfillmentScanner{
		db:         db,
		dispatcher: dispatcher,
		resolver:   resolver,
		sendToEdge: sendFn,
		logFn:      logFn,
		debugLog:   debugLog,
		stopChan:   make(chan struct{}),
	}
}

// Trigger requests a scan. If a scan is already running, the request
// is coalesced — the scanner will re-run after the current scan finishes.
func (s *FulfillmentScanner) Trigger() {
	s.triggerMu.Lock()
	s.pending = true
	s.triggerMu.Unlock()
}

// RunOnce executes a single scan pass. Only one scan runs at a time.
func (s *FulfillmentScanner) RunOnce() int {
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
func (s *FulfillmentScanner) StartPeriodicSweep(interval time.Duration) {
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
func (s *FulfillmentScanner) Stop() {
	close(s.stopChan)
}

func (s *FulfillmentScanner) scan() int {
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

func (s *FulfillmentScanner) tryFulfill(order *store.Order) bool {
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

	payloadCode := order.PayloadCode
	if payloadCode == "" {
		return false
	}

	// Find a matching bin based on order type
	var bin *store.Bin
	var sourceNode *store.Node

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
		if order.PickupNode != "" {
			// Try NGRP resolution first
			pickupNode, pErr := s.db.GetNodeByDotName(order.PickupNode)
			if pErr == nil && pickupNode.IsSynthetic && pickupNode.NodeTypeCode == "NGRP" && s.resolver != nil {
				result, rErr := s.resolver.Resolve(pickupNode, dispatch.OrderTypeRetrieve, payloadCode, nil)
				if rErr == nil {
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

	// Update order with bin and source
	_ = s.db.UpdateOrderBinID(order.ID, bin.ID)
	_ = s.db.UpdateOrderPickupNode(order.ID, sourceNode.Name)
	_ = s.db.UpdateOrderStatus(order.ID, protocol.StatusSourcing, "bin found, dispatching")

	// Resolve destination
	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		s.logFn("fulfillment: dest node %q not found for order %d: %v", order.DeliveryNode, order.ID, err)
		s.db.UnclaimOrderBins(order.ID)
		_ = s.db.UpdateOrderStatus(order.ID, protocol.StatusQueued, "awaiting inventory")
		return false
	}

	// Dispatch to fleet — use DispatchDirect which handles fleet creation.
	// On failure, DispatchDirect sets status to failed. We override back to queued
	// since this is a transient fleet issue, not a permanent failure.
	vendorOrderID, err := s.dispatcher.DispatchDirect(order, sourceNode, destNode)
	if err != nil {
		s.logFn("fulfillment: fleet dispatch failed for order %d, re-queuing: %v", order.ID, err)
		s.db.UnclaimOrderBins(order.ID)
		_ = s.db.UpdateOrderStatus(order.ID, protocol.StatusQueued, "fleet unavailable, re-queued")
		return false
	}

	s.logFn("fulfillment: order %d fulfilled — bin %d (%s -> %s) vendor=%s",
		order.ID, bin.ID, sourceNode.Name, destNode.Name, vendorOrderID)

	// Notify Edge: ack + waybill
	if order.StationID != "" {
		_ = s.sendToEdge(protocol.TypeOrderAck, order.StationID, &protocol.OrderAck{
			OrderUUID:     order.EdgeUUID,
			ShingoOrderID: order.ID,
			SourceNode:    sourceNode.Name,
		})
		_ = s.sendToEdge(protocol.TypeOrderWaybill, order.StationID, &protocol.OrderWaybill{
			OrderUUID: order.EdgeUUID,
			WaybillID: vendorOrderID,
		})
	}

	return true
}
