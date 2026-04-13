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
	if err := d.claimComplexBins(order, resolvedSteps, payloadCode, p.RemainingUOP); err != nil {
		d.failOrder(order, env, "no_bin", err.Error())
		return
	}

	// Split steps at the first "wait" action
	preWait, hasWait := splitAtWait(resolvedSteps)

	// Build RDS blocks for pre-wait steps
	vendorOrderID := fmt.Sprintf("%s%d-%s", VendorIDPrefix, order.ID, uuid.New().String()[:8])
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
		if err := d.backend.ReleaseOrder(vendorOrderID, nil, true); err != nil {
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
// Multi-wait support: the order's WaitIndex tracks how many wait points have
// been consumed. Each release emits only the next segment (steps between
// consecutive waits) and increments the index. The fleet order stays staged
// (complete=false) until the final segment is released.
func (d *Dispatcher) HandleOrderRelease(env *protocol.Envelope, p *protocol.OrderRelease) {
	stationID := env.Src.Station
	d.dbg("order release: station=%s uuid=%s", stationID, p.OrderUUID)

	order, ok := d.getOwnedOrder(env, p.OrderUUID)
	if !ok {
		d.sendError(env, p.OrderUUID, "not_found", "order not found or access denied")
		return
	}

	if order.Status != StatusStaged {
		d.sendError(env, p.OrderUUID, "invalid_state", fmt.Sprintf("order must be staged to release, got %s", order.Status))
		return
	}

	// Parse stored steps
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &steps); err != nil {
		d.sendError(env, p.OrderUUID, "internal_error", "failed to parse stored steps")
		return
	}

	// Extract the next segment: steps after wait N up to wait N+1 (or end).
	segment, moreWaits, blockOffset := splitSegment(steps, order.WaitIndex)
	if segment == nil {
		d.sendError(env, p.OrderUUID, "invalid_state",
			fmt.Sprintf("wait_index %d exceeds number of waits in order", order.WaitIndex))
		return
	}

	// Patch: if the order was redirected while staged, DeliveryNode reflects the
	// new destination but StepsJSON still has the original dropoff node. Replace
	// the last dropoff in the segment with the current DeliveryNode so fleet
	// blocks route to the correct destination. Only patch the final segment
	// (!moreWaits) — intermediate segments have legitimate dropoffs that differ
	// from the final destination.
	if order.DeliveryNode != "" && !moreWaits {
		for i := len(segment) - 1; i >= 0; i-- {
			if segment[i].Action == "dropoff" {
				if segment[i].Node != order.DeliveryNode {
					d.dbg("complex release: patching segment dropoff %s -> %s (redirect)", segment[i].Node, order.DeliveryNode)
					segment[i].Node = order.DeliveryNode
				}
				break
			}
		}
	}

	blocks := stepsToBlocks(order.VendorOrderID, segment, blockOffset)
	complete := !moreWaits

	d.dbg("complex release: order=%d vendor=%s wait_index=%d adding %d blocks complete=%v",
		order.ID, order.VendorOrderID, order.WaitIndex, len(blocks), complete)

	if err := d.backend.ReleaseOrder(order.VendorOrderID, blocks, complete); err != nil {
		log.Printf("dispatch: fleet release order failed: %v", err)
		d.sendError(env, p.OrderUUID, "fleet_failed", err.Error())
		return
	}

	// Advance wait index so the next release picks up the right segment.
	newWaitIndex := order.WaitIndex + 1
	if err := d.db.UpdateOrderWaitIndex(order.ID, newWaitIndex); err != nil {
		log.Printf("dispatch: update order %d wait_index to %d: %v", order.ID, newWaitIndex, err)
	}

	if err := d.db.UpdateOrderStatus(order.ID, StatusInTransit, fmt.Sprintf("released from staging (wait %d)", order.WaitIndex)); err != nil {
		log.Printf("dispatch: update order %d status to in_transit: %v", order.ID, err)
	}
	log.Printf("dispatch: complex order %d released with %d additional blocks (wait %d, complete=%v)",
		order.ID, len(blocks), order.WaitIndex, complete)
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
			// Wait may optionally include a node (drive-to-and-hold).
			// If present, resolve it; otherwise it's a bare wait (split point only).
			if step.Node != "" {
				nodeName, err := d.resolveStepNode(step, payloadCode)
				if err != nil {
					d.sendError(env, orderUUID, "resolution_failed", fmt.Sprintf("step %d: %v", i, err))
					return nil, err
				}
				resolved = append(resolved, resolvedStep{Action: "wait", Node: nodeName})
			} else {
				resolved = append(resolved, resolvedStep{Action: "wait"})
			}
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
				return "", fmt.Errorf("cannot resolve group %s: %w", step.Node, err)
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
				return "", fmt.Errorf("no source bin for payload %q: %w", payloadCode, err)
			}
			node, err := d.db.GetNode(*bin.NodeID)
			if err != nil {
				return "", fmt.Errorf("resolve node for source bin %d: %w", bin.ID, err)
			}
			d.dbg("resolveStepNode: global fallback pickup → %s (bin %d)", node.Name, bin.ID)
			return node.Name, nil
		case "dropoff":
			node, err := d.db.FindStorageDestination(payloadCode)
			if err != nil {
				return "", fmt.Errorf("no storage destination for payload %q: %w", payloadCode, err)
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

// splitAtWait returns steps up to and including the first "wait" and whether a
// wait was found. A wait-with-node produces an RDS block (BinTask=Wait) and is
// included in preWait so the robot receives the "drive to node" instruction
// before the order is staged. A bare wait (no node) is a pure split marker and
// is excluded from preWait (no block emitted).
func splitAtWait(steps []resolvedStep) (preWait []resolvedStep, hasWait bool) {
	for i, s := range steps {
		if s.Action == "wait" {
			if s.Node != "" {
				// Wait-with-node: include it (becomes a Wait block), split after.
				return steps[:i+1], true
			}
			// Bare wait: split before (no block for this step).
			return steps[:i], true
		}
	}
	return steps, false
}

// splitSegment extracts the next segment of steps to release for a given
// waitIndex. It skips past the first (waitIndex+1) wait actions, then returns
// steps up to the next wait (or end of list). Returns the segment, whether
// more waits remain after it, and the block offset (total steps that produce
// RDS blocks before this segment) for correct block ID numbering.
//
// Wait-with-node steps produce RDS blocks (BinTask=Wait) and count toward the
// offset. Bare waits (no node) are pure split markers and do not produce blocks.
//
// Example for steps: [pickup, dropoff, wait(node), pickup, dropoff, wait, pickup, dropoff]
//
//	waitIndex=0 → segment=[pickup, dropoff] after wait₀, moreWaits=true, offset=3
//	waitIndex=1 → segment=[pickup, dropoff] after wait₁, moreWaits=false, offset=5+1
func splitSegment(steps []resolvedStep, waitIndex int) (segment []resolvedStep, moreWaits bool, blockOffset int) {
	// Find the start: skip past (waitIndex+1) wait actions.
	// waitIndex=0 means we want steps after the 1st wait.
	waitsSeen := 0
	startIdx := 0
	found := false
	for i, s := range steps {
		if s.Action == "wait" {
			waitsSeen++
			if waitsSeen == waitIndex+1 {
				startIdx = i + 1
				found = true
				break
			}
		}
	}

	// Guard: if waitIndex exceeds the number of waits in the step list,
	// return an empty segment. This prevents a stale or duplicate release
	// from silently replaying the entire order.
	if !found {
		return nil, false, 0
	}

	// Count steps before startIdx that produce RDS blocks.
	// pickup/dropoff always produce blocks. wait-with-node produces a block
	// (BinTask=Wait). Bare waits (no node) produce no block.
	blockOffset = 0
	for i := 0; i < startIdx; i++ {
		if steps[i].Action != "wait" || steps[i].Node != "" {
			blockOffset++
		}
	}

	// Find the end: next wait after startIdx, or end of steps.
	// A wait-with-node is included in the segment (it produces an RDS block);
	// the split happens after it. A bare wait ends the segment before it.
	endIdx := len(steps)
	for i := startIdx; i < len(steps); i++ {
		if steps[i].Action == "wait" {
			if steps[i].Node != "" {
				// Wait-with-node: include it in segment, split after.
				endIdx = i + 1
			} else {
				// Bare wait: split before.
				endIdx = i
			}
			moreWaits = true
			break
		}
	}

	segment = steps[startIdx:endIdx]
	return
}

// stepsToBlocks converts resolved steps to fleet OrderBlocks.
// blockOffset shifts the block numbering so that post-wait blocks don't
// collide with pre-wait block IDs already submitted to RDS.
func stepsToBlocks(vendorOrderID string, steps []resolvedStep, blockOffset int) []fleet.OrderBlock {
	var blocks []fleet.OrderBlock
	for i, s := range steps {
		if s.Action == "wait" && s.Node == "" {
			// Bare wait (no node) is a split point only — not an RDS block.
			continue
		}
		// Map action to bin task for SEER RDS
		var binTask string
		switch s.Action {
		case "pickup":
			binTask = "JackLoad"
		case "dropoff":
			binTask = "JackUnload"
		case "wait":
			// Wait-with-node: robot drives to the node and holds (RDS Wait key).
			binTask = "Wait"
		}
		blocks = append(blocks, fleet.OrderBlock{
			BlockID:  fmt.Sprintf("%s-b%d", vendorOrderID, blockOffset+i+1),
			Location: s.Node,
			BinTask:  binTask,
		})
	}
	return blocks
}

// claimedBin records which bin was claimed at which pickup step.
type claimedBin struct {
	binID     int64
	stepIndex int
	nodeName  string
}

// claimComplexBins resolves and claims bins for pickup steps in a complex order.
// For single-pickup orders (the most common pattern), it sets Order.BinID so
// that the normal completion flow — ApplyBinArrival (moves bin to delivery
// node in the DB) and maybeCreateReturnOrder (auto-return on cancel/fail) —
// works correctly.
//
// For multi-pickup orders, per-bin destinations are computed via
// resolvePerBinDestinations and recorded in the order_bins junction table.
// handleOrderCompleted uses these rows to move each bin to its correct
// destination instead of blindly using Order.DeliveryNode.
//
// The claim is best-effort: if no unclaimed bin matching the payload is found
// at a pickup node, the order still dispatches (same as prior behavior).
//
// Compound order children (ParentOrderID != nil) never populate the junction
// table — each child is a single-bin order handled by the legacy path.
func (d *Dispatcher) claimComplexBins(order *store.Order, steps []resolvedStep, payloadCode string, remainingUOP *int) error {
	// Determine the process node name from the order's source metadata.
	// Only the outgoing bin at the process node gets remainingUOP applied;
	// all other pickups (e.g. storage pickups) use a plain claim.
	processNode := order.SourceNode

	var claimed []claimedBin
	for i, s := range steps {
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
			// Only apply remainingUOP at the process node (outgoing bin).
			// Storage pickups and other steps get a plain claim (nil).
			var stepUOP *int
			if s.Node == processNode {
				stepUOP = remainingUOP
			}
			if err := d.binManifest.ClaimForDispatch(bin.ID, order.ID, stepUOP); err != nil {
				continue
			}
			d.dbg("complex: claimed bin %d (%s) at %s for order %d",
				bin.ID, bin.Label, s.Node, order.ID)
			d.db.AppendAudit("bin", bin.ID, "claimed",
				"", fmt.Sprintf("complex order %d pickup at %s", order.ID, s.Node), "system")
			claimed = append(claimed, claimedBin{binID: bin.ID, stepIndex: i, nodeName: s.Node})
			break
		}
	}

	if len(claimed) == 0 {
		return &planningError{Code: "no_bin", Detail: fmt.Sprintf("no available bin at pickup node(s) for order %d", order.ID)}
	}

	// Set Order.BinID to the first claimed bin. This enables the standard
	// single-bin completion path in wiring.go for simple complex orders.
	order.BinID = &claimed[0].binID
	if err := d.db.UpdateOrderBinID(order.ID, claimed[0].binID); err != nil {
		log.Printf("dispatch: update complex order %d bin_id: %v", order.ID, err)
	}

	// Multi-bin: populate the order_bins junction table with per-bin destinations.
	// Compound children never use this — each child is a single-bin order.
	if len(claimed) > 1 && order.ParentOrderID == nil {
		// Build the claimedBins map for destination resolution: pickupNode → binID
		claimedMap := make(map[string]int64, len(claimed))
		for _, c := range claimed {
			claimedMap[c.nodeName] = c.binID
		}

		destinations := resolvePerBinDestinations(steps, claimedMap)

		for _, c := range claimed {
			destNode := destinations[c.binID]
			if err := d.db.InsertOrderBin(order.ID, c.binID, c.stepIndex, "pickup", c.nodeName, destNode); err != nil {
				log.Printf("dispatch: insert order_bin for order %d bin %d: %v", order.ID, c.binID, err)
			}
		}

		log.Printf("dispatch: complex order %d has %d pickups — per-bin destinations recorded in order_bins",
			order.ID, len(claimed))
	} else if len(claimed) > 1 {
		log.Printf("dispatch: complex order %d has %d pickups — Order.BinID tracks first bin %d only (compound child, no junction table)",
			order.ID, len(claimed), claimed[0].binID)
	}
	return nil
}

// resolvePerBinDestinations simulates the step sequence to determine where each
// claimed bin ends up after all pickups and dropoffs complete. The bin identity
// is tracked by location: a pickup at node X grabs whichever bin was last
// dropped there.
//
// Returns a map of binID → final destination node name.
//
// Edge cases handled:
//   - Empty robot dropoff (pre-positioning): carrying == 0, dropoff is a no-op
//   - Ghost pickup (no bin at node): carrying stays 0
//   - Bin re-pickup: a bin dropped at staging then picked up again gets a new dest
func resolvePerBinDestinations(steps []resolvedStep, claimedBins map[string]int64) map[int64]string {
	// Which bin the robot is currently carrying (0 = empty)
	var carrying int64

	// Which bin is sitting at which node after being dropped
	binAtNode := make(map[string]int64, len(claimedBins))
	for nodeName, binID := range claimedBins {
		binAtNode[nodeName] = binID
	}

	// Last known dropoff destination per bin
	dest := make(map[int64]string, len(claimedBins))

	for _, step := range steps {
		switch step.Action {
		case "pickup":
			if binID, ok := binAtNode[step.Node]; ok {
				carrying = binID
				delete(binAtNode, step.Node) // bin leaves this node
			}
			// If no bin at this node, robot picks up nothing (ghost/pre-position)

		case "dropoff":
			if carrying != 0 {
				dest[carrying] = step.Node       // update final dest
				binAtNode[step.Node] = carrying  // bin is now at this node
				carrying = 0
			}
			// If robot is empty, this is a pre-position drive (no-op for bin tracking)

		case "wait":
			// No bin movement
		}
	}

	return dest
}
