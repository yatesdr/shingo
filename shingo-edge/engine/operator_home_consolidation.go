// operator_home_consolidation.go — "Clear Loader Home" feature.
//
// Operator workflow: a supermarket reach operator selects a full dedicated-home
// position on the HMI and clicks "Clear Bin". Shingo:
//  1. Zeroes the home bin's UOP (declare it cleared/empty by forklift).
//  2. Dispatches Order A: move empty carrier from home → buffer slot.
//  3. When Order A's robot picks up the empty (home is now clear), dispatches
//     Order B: move buffer partial → home.
//
// Net result: buffer slot holds the empty carrier, home position has the partial.
// The buffer position temporarily holds two obligations (partial out, empty in),
// which Core resolves via its queuing — it holds Order A's delivery until Order B
// frees the slot.
//
// See homeConsolidations on Engine for the pending-state storage.

package engine

import (
	"fmt"
	"log"

	"shingoedge/domain"
)

// homeConsolidation is the pending-state record for an in-flight consolidation
// sequence. Stored under Order A's UUID; consumed by HandleBinPickedUp when
// Order A's robot physically leaves the home position.
type homeConsolidation struct {
	bufferCoreName    string // Core node name of the buffer slot
	homeCoreName      string // Core node name of the dedicated home position
	homeProcessNodeID int64  // Edge process node ID of the home (for Order B tracking)
	payload           string // Payload code of the partial being consolidated
}

// ClearLoaderHome implements the Clear Loader Home operator action. The operator
// declares that the full/partial bin at the given home position has been zeroed
// (UOP = 0), and requests Shingo move the empty out and pull the matching buffer
// partial into the home.
//
// Pre-conditions (enforced here):
//   - nodeID must be a produce, dedicated-home loader position.
//   - The home must hold a tracked bin with UOP > 0.
//   - The loader's buffer position must hold a partial with matching payload.
func (e *Engine) ClearLoaderHome(nodeID int64) error {
	node, _, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return err
	}
	if claim == nil {
		return fmt.Errorf("node %s has no active claim", node.Name)
	}

	l, err := e.loaders().LoaderForNode(domain.NodeID(node.CoreNodeName))
	if err != nil || l == nil {
		return fmt.Errorf("node %s is not a loader position", node.Name)
	}
	if !l.IsDedicated() {
		return fmt.Errorf("node %s is not a dedicated-position loader", node.Name)
	}

	// Verify this node is a home position (has a pinned payload in the loader
	// aggregate). A position with no pinned payload is a buffer slot — not clearable.
	homePayloadPinned, hasPinned := l.PayloadAt(domain.NodeID(node.CoreNodeName))
	if !hasPinned || homePayloadPinned == "" {
		return fmt.Errorf("node %s is a buffer position, not a home position", node.Name)
	}

	// Gather buffer node names: positions whose Payload is "" in the loader aggregate.
	// Exclude the home node itself (guard for loaders where the home has no pinned
	// payload — shouldn't happen given the check above, but defence-in-depth).
	var bufferNodes []string
	for _, pos := range l.Positions() {
		if pos.Payload == "" && string(pos.Node) != node.CoreNodeName {
			bufferNodes = append(bufferNodes, string(pos.Node))
		}
	}
	if len(bufferNodes) == 0 {
		return fmt.Errorf("loader for node %s has no buffer position", node.Name)
	}

	allNodes := append([]string{node.CoreNodeName}, bufferNodes...)
	bins, err := e.coreClient.FetchNodeBins(allNodes)
	if err != nil {
		return fmt.Errorf("clear loader home: fetch bin states: %w", err)
	}

	// Index the results by node name.
	binMap := make(map[string]NodeBinInfo, len(bins))
	for _, b := range bins {
		binMap[b.NodeName] = b
	}

	homeInfo := binMap[node.CoreNodeName]
	if !homeInfo.Occupied || homeInfo.UOPRemaining <= 0 {
		return fmt.Errorf("node %s has no bin with UOP to clear", node.Name)
	}
	homePayload := homeInfo.PayloadCode
	if homePayload == "" {
		return fmt.Errorf("node %s bin has no payload — not a clearable home bin", node.Name)
	}

	// Find a buffer position holding a partial with matching payload.
	var bufferCoreName string
	for _, bName := range bufferNodes {
		info := binMap[bName]
		if info.Occupied && info.PayloadCode == homePayload && info.UOPRemaining > 0 {
			bufferCoreName = bName
			break
		}
	}
	if bufferCoreName == "" {
		return fmt.Errorf("no matching partial for payload %s in buffer — cannot consolidate", homePayload)
	}

	// Zero the home bin's UOP via Core (declare it empty; robot will move the
	// carrier out via Order A).
	if err := e.coreClient.ClearBin(node.CoreNodeName, ""); err != nil {
		return fmt.Errorf("clear loader home: zero bin UOP: %w", err)
	}
	// Mirror the zero into the Edge delta accumulator so the station view reflects
	// the cleared count immediately (same as Engine.ClearBin for produce nodes).
	if e.inventoryDelta != nil {
		var claimIDPtr *int64
		if claim.ID != 0 {
			claimIDPtr = &claim.ID
		}
		if err := e.inventoryDelta.SetClaimAndCount(nodeID, claimIDPtr, 0); err != nil {
			log.Printf("home_consolidation: set delta for node %d: %v", nodeID, err)
		}
	}

	// Order A: move the now-empty carrier from home → buffer.
	// The buffer is currently occupied by the partial we'll move via Order B, so
	// Core will queue this delivery until Order B's pickup frees the slot.
	nodeIDCopy := nodeID
	orderA, err := e.orderMgr.CreateMoveOrderWithPayloadCode(&nodeIDCopy, 1,
		node.CoreNodeName, bufferCoreName, "", true)
	if err != nil {
		return fmt.Errorf("clear loader home: create empty-out order: %w", err)
	}
	log.Printf("home_consolidation: Order A (empty-out) %d: %s → %s", orderA.ID, node.CoreNodeName, bufferCoreName)

	// Register the pending consolidation. HandleBinPickedUp fires Order B when
	// Order A's robot physically picks up the empty (home is now clear to receive
	// the partial).
	e.homeConsolidationsMu.Lock()
	e.homeConsolidations[orderA.UUID] = homeConsolidation{
		bufferCoreName:    bufferCoreName,
		homeCoreName:      node.CoreNodeName,
		homeProcessNodeID: nodeID,
		payload:           homePayload,
	}
	e.homeConsolidationsMu.Unlock()

	return nil
}

// dispatchBufferConsolidation creates Order B: moves the buffer partial to the
// home position now that Order A's pickup has cleared it. Called from
// HandleBinPickedUp when the Order A UUID matches a pending consolidation.
func (e *Engine) dispatchBufferConsolidation(c homeConsolidation) {
	nodeID := c.homeProcessNodeID
	order, err := e.orderMgr.CreateMoveOrderWithPayloadCode(&nodeID, 1,
		c.bufferCoreName, c.homeCoreName, c.payload, true)
	if err != nil {
		e.logFn("home_consolidation: create Order B (buffer→home) %s→%s: %v",
			c.bufferCoreName, c.homeCoreName, err)
		return
	}
	log.Printf("home_consolidation: Order B (partial→home) %d: %s → %s payload=%q",
		order.ID, c.bufferCoreName, c.homeCoreName, c.payload)

	if err := e.db.UpdateProcessNodeRuntimeOrders(c.homeProcessNodeID, &order.ID, nil); err != nil {
		log.Printf("home_consolidation: update runtime for home node %d: %v", c.homeProcessNodeID, err)
	}
}

// EnrichHomeBufferPartials sets HasBufferPartial on any StationNodeView that is a
// dedicated-home loader tile AND has a matching partial in the loader's buffer slot.
// Called from the operator-station view handler after enrichViewBinState so BinState
// is already populated. Makes one FetchNodeBins call across all buffer positions to
// keep the overhead bounded regardless of how many home tiles are on the board.
func (e *Engine) EnrichHomeBufferPartials(nodes []domain.StationNodeView) {
	if !e.coreClient.Available() {
		return
	}

	type homeEntry struct {
		idx     int
		payload string
		buffers []string
	}
	var homes []homeEntry
	seen := make(map[string]bool)
	var allBuffers []string

	for i := range nodes {
		n := &nodes[i]
		if !n.HomeLocationLoader {
			continue
		}
		if n.BinState == nil || !n.BinState.Occupied || n.BinState.UOPRemaining <= 0 || n.BinState.PayloadCode == "" {
			continue
		}
		l, err := e.loaders().LoaderForNode(domain.NodeID(n.Node.CoreNodeName))
		if err != nil || l == nil || !l.IsDedicated() {
			continue
		}
		// Skip buffer positions (no pinned payload in the loader aggregate). They
		// are not homes and should not be candidates for consolidation.
		pinnedPayload, hasPinned := l.PayloadAt(domain.NodeID(n.Node.CoreNodeName))
		if !hasPinned || pinnedPayload == "" {
			continue
		}
		var bufs []string
		for _, pos := range l.Positions() {
			if pos.Payload != "" {
				continue
			}
			name := string(pos.Node)
			if name == n.Node.CoreNodeName {
				continue // don't list self as a buffer
			}
			bufs = append(bufs, name)
			if !seen[name] {
				seen[name] = true
				allBuffers = append(allBuffers, name)
			}
		}
		if len(bufs) == 0 {
			continue
		}
		homes = append(homes, homeEntry{i, n.BinState.PayloadCode, bufs})
	}

	if len(homes) == 0 {
		return
	}

	bins, err := e.coreClient.FetchNodeBins(allBuffers)
	if err != nil {
		return
	}
	bufMap := make(map[string]NodeBinInfo, len(bins))
	for _, b := range bins {
		bufMap[b.NodeName] = b
	}

	for _, h := range homes {
		for _, bName := range h.buffers {
			info := bufMap[bName]
			if info.Occupied && info.PayloadCode == h.payload && info.UOPRemaining > 0 {
				nodes[h.idx].HasBufferPartial = true
				break
			}
		}
	}
}
