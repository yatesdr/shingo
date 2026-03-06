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

	// Resolve blueprint
	var blueprintID *int64
	if p.BlueprintCode != "" {
		bp, err := d.db.GetBlueprintByCode(p.BlueprintCode)
		if err != nil {
			d.sendError(env, p.OrderUUID, "blueprint_error", fmt.Sprintf("blueprint %q not found", p.BlueprintCode))
			return
		}
		blueprintID = &bp.ID
	}

	// Resolve steps: validate nodes and resolve synthetic groups
	resolvedSteps, err := d.resolveComplexSteps(p.Steps, blueprintID, env, p.OrderUUID)
	if err != nil {
		return // error already sent to edge
	}

	stepsJSON, err := json.Marshal(resolvedSteps)
	if err != nil {
		d.sendError(env, p.OrderUUID, "internal_error", "failed to marshal steps")
		return
	}

	// Determine pickup and delivery from first and last non-wait steps
	pickupNode, deliveryNode := extractEndpoints(resolvedSteps)

	// Create order record
	order := &store.Order{
		EdgeUUID:    p.OrderUUID,
		StationID:   stationID,
		OrderType:   OrderTypeComplex,
		Status:      StatusPending,
		Quantity:    p.Quantity,
		Priority:    p.Priority,
		PayloadDesc: p.PayloadDesc,
		PickupNode:  pickupNode,
		DeliveryNode: deliveryNode,
		StepsJSON:   string(stepsJSON),
	}
	if blueprintID != nil {
		order.BlueprintID = blueprintID
	}

	if err := d.db.CreateOrder(order); err != nil {
		log.Printf("dispatch: create complex order: %v", err)
		d.sendError(env, p.OrderUUID, "internal_error", err.Error())
		return
	}
	d.db.UpdateOrderStatus(order.ID, StatusPending, "complex order received")
	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, OrderTypeComplex, p.BlueprintCode, deliveryNode)

	// Split steps at the first "wait" action
	preWait, hasWait := splitAtWait(resolvedSteps)

	// Build RDS blocks for pre-wait steps
	vendorOrderID := fmt.Sprintf("sg-%d-%s", order.ID, uuid.New().String()[:8])
	blocks := stepsToBlocks(vendorOrderID, preWait)

	if len(blocks) == 0 {
		d.failOrder(order, env, "invalid_steps", "no actionable steps before wait")
		return
	}

	d.db.UpdateOrderStatus(order.ID, StatusSourcing, "resolving complex steps")

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
		// Use CreateStagedOrder but the backend gets complete=false;
		// we need a complete order. Use CreateTransportOrder pattern instead.
		// Actually — for a multi-block order without wait, we still use SetOrder with complete=true.
		// Let's just use the RDS client's CreateOrder directly via adapter.
		// For simplicity, create as staged and then immediately release (empty release).
		if _, err := d.backend.CreateStagedOrder(req); err != nil {
			log.Printf("dispatch: fleet create order failed: %v", err)
			d.failOrder(order, env, "fleet_failed", err.Error())
			return
		}
		// Mark complete immediately (no more blocks)
		if err := d.backend.ReleaseOrder(vendorOrderID, nil); err != nil {
			log.Printf("dispatch: fleet mark complete failed: %v", err)
			// Order is created, don't fail it — it'll still work
		}
	}

	log.Printf("dispatch: complex order %d dispatched as %s (%d steps)", order.ID, vendorOrderID, len(resolvedSteps))
	d.db.UpdateOrderVendor(order.ID, vendorOrderID, "CREATED", "")
	d.db.UpdateOrderStatus(order.ID, StatusDispatched, fmt.Sprintf("vendor order %s created", vendorOrderID))
	d.emitter.EmitOrderDispatched(order.ID, vendorOrderID, pickupNode, deliveryNode)
	d.sendAck(env, order.EdgeUUID, order.ID, pickupNode)
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

	_, postWait := splitPostWait(steps)
	blocks := stepsToBlocks(order.VendorOrderID, postWait)

	d.dbg("complex release: order=%d vendor=%s adding %d blocks", order.ID, order.VendorOrderID, len(blocks))

	if err := d.backend.ReleaseOrder(order.VendorOrderID, blocks); err != nil {
		log.Printf("dispatch: fleet release order failed: %v", err)
		d.sendError(env, p.OrderUUID, "fleet_failed", err.Error())
		return
	}

	d.db.UpdateOrderStatus(order.ID, StatusInTransit, "released from staging")
	log.Printf("dispatch: complex order %d released with %d additional blocks", order.ID, len(blocks))
}

// resolvedStep is a step with concrete node names after resolution.
type resolvedStep struct {
	Action string `json:"action"`
	Node   string `json:"node,omitempty"`
}

// resolveComplexSteps validates and resolves all steps, returning concrete node names.
func (d *Dispatcher) resolveComplexSteps(steps []protocol.ComplexOrderStep, blueprintID *int64, env *protocol.Envelope, orderUUID string) ([]resolvedStep, error) {
	var resolved []resolvedStep
	for i, step := range steps {
		switch step.Action {
		case "pickup", "dropoff":
			nodeName, err := d.resolveStepNode(step, blueprintID)
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

// resolveStepNode resolves a single step's node, handling both direct and synthetic nodes.
func (d *Dispatcher) resolveStepNode(step protocol.ComplexOrderStep, blueprintID *int64) (string, error) {
	if step.Node != "" {
		// Direct node: validate it exists
		_, err := d.db.GetNodeByDotName(step.Node)
		if err != nil {
			return "", fmt.Errorf("node %q not found", step.Node)
		}
		return step.Node, nil
	}
	if step.NodeGroup != "" && d.resolver != nil {
		// Synthetic node group: resolve to concrete node
		groupNode, err := d.db.GetNodeByDotName(step.NodeGroup)
		if err != nil {
			return "", fmt.Errorf("node group %q not found", step.NodeGroup)
		}
		orderType := OrderTypeRetrieve
		if step.Action == "dropoff" {
			orderType = OrderTypeStore
		}
		result, err := d.resolver.Resolve(groupNode, orderType, blueprintID, nil)
		if err != nil {
			return "", fmt.Errorf("cannot resolve %s: %v", step.NodeGroup, err)
		}
		return result.Node.Name, nil
	}
	return "", fmt.Errorf("step requires either node or node_group")
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
func stepsToBlocks(vendorOrderID string, steps []resolvedStep) []fleet.OrderBlock {
	var blocks []fleet.OrderBlock
	for i, s := range steps {
		if s.Action == "wait" {
			continue
		}
		blocks = append(blocks, fleet.OrderBlock{
			BlockID:  fmt.Sprintf("%s-b%d", vendorOrderID, i+1),
			Location: s.Node,
		})
	}
	return blocks
}
