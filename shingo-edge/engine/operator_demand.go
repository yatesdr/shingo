package engine

import (
	"log"

	"shingo/protocol"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// HandleDemandSignal processes a kanban demand signal from Core. It finds
// the local manual_swap node matching the signal's CoreNodeName and triggers
// tryAutoRequest to create orders for the demanded payload if none exist.
func (e *Engine) HandleDemandSignal(signal *protocol.DemandSignal) {
	matches := e.findManualSwapNodes(signal.CoreNodeName)
	if len(matches) == 0 {
		log.Printf("demand signal: no matching manual_swap node for %s", signal.CoreNodeName)
		return
	}
	// matches[0] preserves the original first-match-return behavior — the iteration
	// order in findManualSwapNodes matches the original loop order (processes → claims → nodes).
	m := matches[0]
	e.tryAutoRequest(&m.node, &m.claim)
	log.Printf("demand signal: triggered auto-request for node %s (payload %s, role %s)",
		m.node.Name, signal.PayloadCode, signal.Role)
}

// SendClaimSync builds a ClaimSync message from all manual_swap claims across
// all active processes and sends it to Core. Core uses this to populate its
// demand registry for kanban wiring.
//
// Called on startup (after registration ack) and whenever the operator
// upserts or deletes a style node claim via the admin UI (see
// shingo-edge/www/handlers_api_config.go). Without the UI-triggered
// resync, demand_registry would only converge on the next heartbeat
// cycle or Edge restart.
func (e *Engine) SendClaimSync() {
	stationID := e.cfg.StationID()
	processes, err := e.db.ListProcesses()
	if err != nil {
		log.Printf("claim sync: list processes: %v", err)
		return
	}

	var claims []protocol.ClaimSyncEntry
	for _, proc := range processes {
		if proc.ActiveStyleID == nil {
			continue
		}
		nodeClaims, err := e.db.ListStyleNodeClaims(*proc.ActiveStyleID)
		if err != nil {
			log.Printf("claim sync: list claims for style %d: %v", *proc.ActiveStyleID, err)
			continue
		}
		for _, c := range nodeClaims {
			if c.SwapMode != "manual_swap" {
				continue
			}
			payloads := c.AllowedPayloads()
			if len(payloads) == 0 {
				continue
			}
			claims = append(claims, protocol.ClaimSyncEntry{
				CoreNodeName:        c.CoreNodeName,
				Role:                c.Role,
				AllowedPayloadCodes: payloads,
				OutboundDestination: c.OutboundDestination,
			})
		}
	}

	sync := &protocol.ClaimSync{
		StationID: stationID,
		Claims:    claims,
	}

	env, err := protocol.NewDataEnvelope(
		protocol.SubjectClaimSync,
		protocol.Address{Role: protocol.RoleEdge, Station: stationID},
		protocol.Address{Role: protocol.RoleCore},
		sync,
	)
	if err != nil {
		log.Printf("claim sync: build envelope: %v", err)
		return
	}
	if err := e.SendEnvelope(env); err != nil {
		log.Printf("claim sync: send: %v", err)
		return
	}
	log.Printf("claim sync: sent %d claims to core", len(claims))
}

// manualSwapNode pairs a manual_swap claim with its matching process node.
type manualSwapNode struct {
	node  processes.Node
	claim processes.NodeClaim
}

// findManualSwapNodes returns all (node, claim) pairs where the claim has
// SwapMode=="manual_swap" across all active processes. If coreNodeName is
// non-empty, only nodes matching that name are returned.
func (e *Engine) findManualSwapNodes(coreNodeName string) []manualSwapNode {
	processList, err := e.db.ListProcesses()
	if err != nil {
		log.Printf("findManualSwapNodes: list processes: %v", err)
		return nil
	}

	var results []manualSwapNode
	for _, proc := range processList {
		if proc.ActiveStyleID == nil {
			continue
		}
		claims, err := e.db.ListStyleNodeClaims(*proc.ActiveStyleID)
		if err != nil {
			log.Printf("findManualSwapNodes: list claims for style %d: %v", *proc.ActiveStyleID, err)
			continue
		}

		// Fetch nodes once per process, not once per claim (fixes pre-existing N+1).
		var nodes []processes.Node
		var nodesFetched bool
		for _, claim := range claims {
			if claim.SwapMode != "manual_swap" {
				continue
			}
			if coreNodeName != "" && claim.CoreNodeName != coreNodeName {
				continue
			}
			if !nodesFetched {
				nodes, err = e.db.ListProcessNodesByProcess(proc.ID)
				if err != nil {
					log.Printf("findManualSwapNodes: list nodes for process %d: %v", proc.ID, err)
					break
				}
				nodesFetched = true
			}
			for _, node := range nodes {
				if node.CoreNodeName != claim.CoreNodeName {
					continue
				}
				results = append(results, manualSwapNode{node: node, claim: claim})
			}
		}
	}
	return results
}

// StartupSweepManualSwap iterates all manual_swap claims and ensures orders
// exist for every allowed payload. This kick-starts the kanban loop after a
// restart — tryAutoRequest is purely event-driven and won't fire until an
// order completes, so without this sweep, a freshly restarted Edge would have
// no active demand at manual_swap nodes.
//
// Called from the SetRegisteredHandler callback (after SendClaimSync) so that
// sendFn is wired and Core registration is confirmed.
func (e *Engine) StartupSweepManualSwap() {
	matches := e.findManualSwapNodes("") // all manual_swap nodes
	for i := range matches {
		e.tryAutoRequest(&matches[i].node, &matches[i].claim)
	}
	if len(matches) > 0 {
		log.Printf("startup sweep: checked %d manual_swap node(s)", len(matches))
	}
}

// tryAutoRequest creates orders for all allowed payloads that don't already have
// a pending order at this node. Role-aware: produce (loader) requests empties,
// consume (unloader) requests fulls. Fails silently on any error — the next
// trigger will retry.
//
// The check+create is wrapped in BEGIN IMMEDIATE to prevent two concurrent
// goroutines from both seeing "no existing orders" and creating duplicates.
func (e *Engine) tryAutoRequest(node *processes.Node, claim *processes.NodeClaim) {
	payloads := claim.AllowedPayloads()
	if len(payloads) == 0 {
		return
	}

	// BEGIN IMMEDIATE serializes concurrent access at the SQLite level.
	// If another goroutine holds a write lock, this blocks until it's done.
	if _, err := e.db.Exec("BEGIN IMMEDIATE"); err != nil {
		log.Printf("manual_swap auto-request: begin tx for node %s: %v", node.Name, err)
		return
	}
	defer func() {
			if _, err := e.db.Exec("ROLLBACK"); err != nil {
				log.Printf("manual_swap auto-request: rollback for node %s: %v", node.Name, err)
			}
		}() // no-op if committed

	// Check which payloads already have active (non-terminal) orders at this node.
	existing, _ := e.db.ListActiveOrdersByProcessNode(node.ID)

	// Build a set of payloads that already have pending orders.
	existingPayloads := make(map[string]bool)
	for _, o := range existing {
		if o.PayloadCode != "" {
			existingPayloads[o.PayloadCode] = true
		}
	}

	// Note: previously we short-circuited when any in-flight order had no
	// payload_code, on the assumption it was a legacy/manual record that
	// might collide. That guard blocked every kanban request behind a single
	// stuck empty-payload order (e.g. a manually-submitted move) and never
	// re-opened. We now rely on the per-payload existing check below: an
	// empty-payload order is simply ignored, and each `payload` we own is
	// evaluated on its own merits.

	var created int
	for _, pc := range payloads {
		if existingPayloads[pc] {
			continue // already have an order for this payload
		}
		var order *orders.Order
		var err error
		if claim.Role == "consume" {
			order, err = e.RequestFullBin(node.ID, pc)
		} else {
			order, err = e.RequestEmptyBin(node.ID, pc)
		}
		if err != nil {
			log.Printf("manual_swap auto-request for node %s payload %s: %v", node.Name, pc, err)
			continue
		}
		created++
		log.Printf("manual_swap auto-request: created order %d for node %s (payload %s, role %s)",
			order.ID, node.Name, pc, claim.Role)
	}
	if created > 0 {
		if _, err := e.db.Exec("COMMIT"); err != nil {
			log.Printf("manual_swap auto-request: commit for node %s: %v", node.Name, err)
		}
	}
}
