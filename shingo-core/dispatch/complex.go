package dispatch

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/store"
)

// HandleComplexOrderRequest processes a multi-step transport order from edge.
func (d *Dispatcher) HandleComplexOrderRequest(env *protocol.Envelope, p *protocol.ComplexOrderRequest) {
	stationID := env.Src.Station
	d.dbg("complex order request: station=%s uuid=%s steps=%d", stationID, p.OrderUUID, len(p.Steps))

	if len(p.Steps) == 0 {
		d.sendError(env, p.OrderUUID, "invalid_steps", "complex order requires at least one step")
		return
	}

	// Resolve payload template
	payloadCode := p.PayloadCode

	// Resolve steps: validate nodes and resolve synthetic groups
	resolvedSteps, err := d.resolveComplexSteps(p.Steps, payloadCode, env, p.OrderUUID)
	if err != nil {
		return // error already sent to edge
	}

	stepsJSON, err := json.Marshal(resolvedSteps)
	if err != nil {
		d.sendError(env, p.OrderUUID, "internal_error", "failed to marshal steps")
		return
	}

	// Determine pickup and delivery from first and last non-wait steps
	sourceNode, deliveryNode := extractEndpoints(resolvedSteps)

	// Create order record
	order := &store.Order{
		EdgeUUID:     p.OrderUUID,
		StationID:    stationID,
		OrderType:    OrderTypeComplex,
		Status:       StatusPending,
		Quantity:     p.Quantity,
		Priority:     p.Priority,
		PayloadDesc:  p.PayloadDesc,
		SourceNode:   sourceNode,
		DeliveryNode: deliveryNode,
		StepsJSON:    string(stepsJSON),
	}

	if err := d.db.CreateOrder(order); err != nil {
		log.Printf("dispatch: create complex order: %v", err)
		d.sendError(env, p.OrderUUID, "internal_error", err.Error())
		return
	}
	if err := d.db.UpdateOrderStatus(order.ID, StatusPending, "complex order received"); err != nil {
		log.Printf("dispatch: update order %d status to pending: %v", order.ID, err)
	}
	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, OrderTypeComplex, payloadCode, deliveryNode)

	// Claim bins at pickup nodes so they are protected from poaching
	// while the robot is en route. This closes the gap where complex orders
	// bypassed the ClaimBin call that simple orders make during planning.
	d.claimComplexBins(order, resolvedSteps, payloadCode)

	// Split steps at the first "wait" action
	preWait, hasWait := splitAtWait(resolvedSteps)

	// Build RDS blocks for pre-wait steps
	vendorOrderID := fmt.Sprintf("sg-%d-%s", order.ID, uuid.New().String()[:8])
	blocks := stepsToBlocks(vendorOrderID, preWait, 0)

	if len(blocks) == 0 {
		d.failOrder(order, env, "invalid_steps", "no actionable steps before wait")
		return
	}

	if err := d.db.UpdateOrderStatus(order.ID, StatusSourcing, "resolving complex steps"); err != nil {
		log.Printf("dispatch: update order %d status to sourcing: %v", order.ID, err)
	}

	if hasWait {
		// Incremental order: send initial blocks with complete=false
		req := fleet.StagedOrderRequest{
			OrderID:    vendorOrderID,
			ExternalID: order.EdgeUUID,
			Blocks:     blocks,
			Priority:   order.Priority,
		}
		d.dbg("complex: creating staged order %s with %d initial blocks", vendorOrderID, len(blocks))
		if _, err := d.backend.CreateStagedOrder(req); err != nil {
			log.Printf("dispatch: fleet create staged order failed: %v", err)
			d.failOrder(order, env, "fleet_failed", err.Error())
			return
		}
	} else {
		// No wait: send all blocks as a complete order
		req := fleet.StagedOrderRequest{
			OrderID:    vendorOrderID,
			ExternalID: order.EdgeUUID,
			Blocks:     blocks,
			Priority:   order.Priority,
		}
		if _, err := d.backend.CreateStagedOrder(req); err != nil {
			log.Printf("dispatch: fleet create order failed: %v", err)
			d.failOrder(order, env, "fleet_failed", err.Error())
			return
		}
		// Mark complete immediately (no more blocks)
		if err := d.backend.ReleaseOrder(vendorOrderID, nil); err != nil {
			log.Printf("dispatch: fleet mark complete failed: %v", err)
		}
	}

	log.Printf("dispatch: complex order %d dispatched as %s (%d steps)", order.ID, vendorOrderID, len(resolvedSteps))
	if err := d.db.UpdateOrderVendor(order.ID, vendorOrderID, "CREATED", ""); err != nil {
		log.Printf("dispatch: update order %d vendor: %v", order.ID, err)
	}
	if err := d.db.UpdateOrderStatus(order.ID, StatusDispatched, fmt.Sprintf("vendor order %s created", vendorOrderID)); err != nil {
		log.Printf("dispatch: update order %d status to dispatched: %v", order.ID, err)
	}
	d.emitter.EmitOrderDispatched(order.ID, vendorOrderID, sourceNode, deliveryNode)
	d.sendAck(env, order.EdgeUUID, order.ID, sourceNode)
}

// HandleOrderRelease processes a release request for a staged (dwelling) order.
func (d *Dispatcher) HandleOrderRelease(env *protocol.Envelope, p *protocol.OrderRelease) {
	stationID := env.Src.Station
	d.dbg("order release: station=%s uuid=%s", stationID, p.OrderUUID)

	order, err := d.db.GetOrderByUUID(p.OrderUUID)
	if err != nil {
		log.Printf("dispatch: release order %s not found: %v", p.OrderUUID, err)
		d.sendError(env, p.OrderUUID, "not_found", "order not found")
		return
	}

	if !d.checkOwnership(env, order) {
		d.sendError(env, p.OrderUUID, "forbidden", "station does not own this order")
		return
	}

	if order.Status != StatusStaged {
		d.sendError(env, p.OrderUUID, "invalid_state", fmt.Sprintf("order must be staged to release, got %s", order.Status))
		return
	}

	// Parse stored steps to find post-wait blocks
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &steps); err != nil {
		d.sendError(env, p.OrderUUID, "internal_error", "failed to parse stored steps")
		return
	}

	preWait, postWait := splitPostWait(steps)
	blocks := stepsToBlocks(order.VendorOrderID, postWait, len(preWait)+1)

	d.dbg("complex release: order=%d vendor=%s adding %d blocks", order.ID, order.VendorOrderID, len(blocks))

	if err := d.backend.ReleaseOrder(order.VendorOrderID, blocks); err != nil {
		log.Printf("dispatch: fleet release order failed: %v", err)
		d.sendError(env, p.OrderUUID, "fleet_failed", err.Error())
		return
	}

	if err := d.db.UpdateOrderStatus(order.ID, StatusInTransit, "released from staging"); err != nil {
		log.Printf("dispatch: update order %d status to in_transit: %v", order.ID, err)
	}
	log.Printf("dispatch: complex order %d released with %d additional blocks", order.ID, len(blocks))
}

// resolvedStep is a step with concrete node names after resolution.
type resolvedStep struct {
	Action string `json:"action"`
	Node   string `json:"node,omitempty"`
}

// resolveComplexSteps validates and resolves all steps, returning concrete node names.
func (d *Dispatcher) resolveComplexSteps(steps []protocol.ComplexOrderStep, payloadCode string, env *protocol.Envelope, orderUUID string) ([]resolvedStep, error) {
	var resolved []resolvedStep
	for i, step := range steps {
		switch step.Action {
		case "pickup", "dropoff":
			nodeName, err := d.resolveStepNode(step, payloadCode)
			if err != nil {
				d.sendError(env, orderUUID, "resolution_failed", fmt.Sprintf("step %d: %v", i, err))
				return nil, err
			}
			resolved = append(resolved, resolvedStep{Action: step.Action, Node: nodeName})
		case "wait":
			resolved = append(resolved, resolvedStep{Action: "wait"})
		default:
			err := fmt.Errorf("unknown step action: %s", step.Action)
			d.sendError(env, orderUUID, "invalid_steps", fmt.Sprintf("step %d: %v", i, err))
			return nil, err
		}
	}
	return resolved, nil
}

// resolveStepNode resolves a single step's node. If the node is a synthetic
// group (NGRP), it is automatically resolved via the group resolver. If the
// node is concrete, it is returned directly. If no node is provided, the
// global fallback resolves via payload code.
func (d *Dispatcher) resolveStepNode(step protocol.ComplexOrderStep, payloadCode string) (string, error) {
	if step.Node != "" {
		node, err := d.db.GetNodeByDotName(step.Node)
		if err != nil {
			return "", fmt.Errorf("node %q not found", step.Node)
		}
		// Auto-detect group nodes and resolve to a concrete slot
		if node.IsSynthetic && node.NodeTypeCode == "NGRP" && d.resolver != nil {
			orderType := OrderTypeRetrieve
			if step.Action == "dropoff" {
				orderType = OrderTypeStore
			}
			result, err := d.resolver.Resolve(node, orderType, payloadCode, nil)
			if err != nil {
				return "", fmt.Errorf("cannot resolve group %s: %v", step.Node, err)
			}
			return result.Node.Name, nil
		}
		return node.Name, nil
	}
	// Global fallback: when Edge sends no node, resolve using the payload
	// code — same approach simple orders use via FindSourceBinFIFO (retrieve)
	// and FindStorageDestination (store).
	if payloadCode != "" {
		switch step.Action {
		case "pickup":
			bin, err := d.db.FindSourceBinFIFO(payloadCode)
			if err != nil {
				return "", fmt.Errorf("no source bin for payload %q: %v", payloadCode, err)
			}
			node, err := d.db.GetNode(*bin.NodeID)
			if err != nil {
				return "", fmt.Errorf("resolve node for source bin %d: %v", bin.ID, err)
			}
			d.dbg("resolveStepNode: global fallback pickup → %s (bin %d)", node.Name, bin.ID)
			return node.Name, nil
		case "dropoff":
			node, err := d.db.FindStorageDestination(payloadCode)
			if err != nil {
				return "", fmt.Errorf("no storage destination for payload %q: %v", payloadCode, err)
			}
			d.dbg("resolveStepNode: global fallback dropoff → %s", node.Name)
			return node.Name, nil
		}
	}
	return "", fmt.Errorf("step requires either node or payload_code for resolution")
}

// extractEndpoints returns the pickup (first actionable) and delivery (last actionable) nodes.
func extractEndpoints(steps []resolvedStep) (pickup, delivery string) {
	for _, s := range steps {
		if s.Action == "pickup" || s.Action == "dropoff" {
			if pickup == "" {
				pickup = s.Node
			}
			delivery = s.Node
		}
	}
	return
}

// splitAtWait returns steps before the first "wait" and whether a wait was found.
func splitAtWait(steps []resolvedStep) (preWait []resolvedStep, hasWait bool) {
	for i, s := range steps {
		if s.Action == "wait" {
			return steps[:i], true
		}
	}
	return steps, false
}

// splitPostWait returns steps before and after the first "wait".
func splitPostWait(steps []resolvedStep) (preWait, postWait []resolvedStep) {
	for i, s := range steps {
		if s.Action == "wait" {
			return steps[:i], steps[i+1:]
		}
	}
	return steps, nil
}

// stepsToBlocks converts resolved steps to fleet OrderBlocks.
// blockOffset shifts the block numbering so that post-wait blocks don't
// collide with pre-wait block IDs already submitted to RDS.
func stepsToBlocks(vendorOrderID string, steps []resolvedStep, blockOffset int) []fleet.OrderBlock {
	var blocks []fleet.OrderBlock
	for i, s := range steps {
		if s.Action == "wait" {
			continue
		}
		// Map action to bin task for SEER RDS
		var binTask string
		switch s.Action {
		case "pickup":
			binTask = "JackLoad"
		case "dropoff":
			binTask = "JackUnload"
		}
		blocks = append(blocks, fleet.OrderBlock{
			BlockID:  fmt.Sprintf("%s-b%d", vendorOrderID, blockOffset+i+1),
			Location: s.Node,
			BinTask:  binTask,
		})
	}
	return blocks
}

// claimComplexBins resolves and claims bins for pickup steps in a complex order.
// For single-pickup orders (the most common pattern), it sets Order.BinID so
// that the normal completion flow — ApplyBinArrival (moves bin to delivery
// node in the DB) and maybeCreateReturnOrder (auto-return on cancel/fail) —
// works correctly.
//
// The claim is best-effort: if no unclaimed bin matching the payload is found
// at a pickup node, the order still dispatches (same as prior behavior).
// Multi-pickup orders set BinID to the first claimed bin and log an advisory.
func (d *Dispatcher) claimComplexBins(order *store.Order, steps []resolvedStep, payloadCode string) {
	var claimed []int64
	for _, s := range steps {
		if s.Action != "pickup" {
			continue
		}
		node, err := d.db.GetNodeByDotName(s.Node)
		if err != nil {
			d.dbg("complex: cannot resolve pickup node %s for claiming: %v", s.Node, err)
			continue
		}
		bins, err := d.db.ListBinsByNode(node.ID)
		if err != nil {
			d.dbg("complex: cannot list bins at %s for claiming: %v", s.Node, err)
			continue
		}
		for _, bin := range bins {
			if bin.ClaimedBy != nil {
				continue
			}
			if payloadCode != "" && bin.PayloadCode != payloadCode {
				continue
			}
			// Skip bins that are not available for dispatch.
			// NOTE: "staged" is intentionally NOT excluded here. Complex orders
			// pick up from core nodes and staging lanes where bins are always
			// staged (set by ApplyBinArrival for non-storage slots). Excluding
			// staged would prevent claiming any bin at a production node.
			// Contrast with FindSourceBinFIFO which correctly excludes staged
			// because it only searches storage slots where available is expected.
			switch bin.Status {
			case "maintenance", "flagged", "retired", "quality_hold":
				continue
			}
			if err := d.db.ClaimBin(bin.ID, order.ID); err != nil {
				continue
			}
			d.dbg("complex: claimed bin %d (%s) at %s for order %d",
				bin.ID, bin.Label, s.Node, order.ID)
			d.db.AppendAudit("bin", bin.ID, "claimed",
				"", fmt.Sprintf("complex order %d pickup at %s", order.ID, s.Node), "system")
			claimed = append(claimed, bin.ID)
			break
		}
	}

	if len(claimed) == 0 {
		d.dbg("complex: no bins claimed for order %d — pickup nodes may have no available bins", order.ID)
		return
	}

	// Set Order.BinID to the first claimed bin. This enables the standard
	// completion path in wiring.go (ApplyBinArrival, auto-return on cancel).
	order.BinID = &claimed[0]
	if err := d.db.UpdateOrderBinID(order.ID, claimed[0]); err != nil {
		log.Printf("dispatch: update complex order %d bin_id: %v", order.ID, err)
	}
	if len(claimed) > 1 {
		log.Printf("dispatch: complex order %d has %d pickups — Order.BinID tracks first bin %d only",
			order.ID, len(claimed), claimed[0])
	}
}
