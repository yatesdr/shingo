package engine

// operator_quality_hold.go — the operator side of quality containment (v100).
//
// Three verbs, all riding machinery that already exists:
//
//	SendBinToQualityHold   — the station's "Send to Quality Hold": the bin
//	                       standing at a produce node goes to the claim's
//	                       containment destination, NOW, as an explicit move
//	                       order. The move IS the containment; the Core hold
//	                       marker (best-effort, logged on failure) is the
//	                       audit and the re-release protection.
//	ReleaseFromContainment — the containment screen's "Verify Good": a
//	                       verified bin walks from the containment node to
//	                       the claim's ordinary outbound (the FG drop). The
//	                       hold marker clears with it.
//	RecallContainedPayload — the containment screen's recall: bins of a
//	                       contained payload still sitting at their claims'
//	                       FG outbound nodes get walked into containment.
//	                       Covers what landed at FG before (or during) the
//	                       flag flipping on — in-transit bins land at FG by
//	                       design and are picked up here.
//
// SCOPE NOTE: the divert itself is Core's (dispatch/ placeForContainment),
// keyed on the payload flag. These verbs are the human half: one specific
// bin parked or returned. Both directions are ordinary move orders, so the
// order page, the runtime pointer and the provenance machinery treat them
// like any operator move — which is the point.

import (
	"fmt"
	"log"
	"slices"
	"strings"

	"shingo/protocol"
	ordermgr "shingoedge/orders"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// SendBinToQualityHold parks the bin standing at a produce node into its
// claim's containment destination. See the file header for the shape.
func (e *Engine) SendBinToQualityHold(nodeID int64, actor string) (*orders.Order, error) {
	node, _, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return nil, err
	}
	if node == nil {
		return nil, fmt.Errorf("node %d not found", nodeID)
	}
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	// A synthesized Core-loader claim (ID==0) is a loader position, not a
	// produce node — the button is not offered there, and this refusal is the
	// API-side backstop.
	if claim.ID == 0 {
		return nil, fmt.Errorf("node %s is a Core-owned loader position — quality hold does not apply", node.Name)
	}
	containment := claim.ContainmentDestination
	if containment == "" {
		return nil, fmt.Errorf("node %s's claim declares no containment destination — configure the claim's Quality Containment section first", node.Name)
	}
	if containment == node.CoreNodeName {
		return nil, fmt.Errorf("node %s's containment destination is itself — misconfigured claim", node.Name)
	}
	if actor == "" {
		actor = "operator"
	}

	// The bin standing at the node, from the same read the station screen
	// renders. No bin, nothing to hold.
	binsAtNode, _, err := e.coreClient.FetchNodeBins([]string{node.CoreNodeName})
	if err != nil {
		return nil, fmt.Errorf("read bin at node: %w", err)
	}
	if len(binsAtNode) == 0 || !binsAtNode[0].Occupied {
		return nil, fmt.Errorf("no bin at node %s to hold", node.Name)
	}
	bin := binsAtNode[0]

	// Double-tap guard (the U2 empty-out guard's shape): the order layer has
	// no dedup for move orders, so a second tap while one is in flight would
	// send two robots for one carrier.
	if existing, lerr := e.db.ListActiveOrdersByProcessNodeAndType(nodeID, protocol.OrderTypeMove); lerr != nil {
		return nil, fmt.Errorf("check in-flight move: %w", lerr)
	} else if len(existing) > 0 {
		return nil, fmt.Errorf("a move order (%d) is already in flight for node %s", existing[0].ID, node.Name)
	}

	nodeIDCopy := node.ID
	order, err := e.orderMgr.CreateMoveOrderWithPayloadCode(&nodeIDCopy, 1, node.CoreNodeName, containment, bin.PayloadCode, true,
		ordermgr.NoDemand())
	if err != nil {
		return nil, fmt.Errorf("create containment move: %w", err)
	}
	// The marker is best-effort: the move IS the containment, so a failed
	// marker write logs and continues rather than telling an operator their
	// hold failed when the robot is already coming.
	if err := e.coreClient.SetBinQualityHold(bin.BinID, true, actor); err != nil {
		log.Printf("quality-hold: move %d created but the Core hold marker failed for bin %d: %v", order.ID, bin.BinID, err)
	}
	// Point the runtime at the move so the station shows it next, the way the
	// U2 empty-out does.
	if err := e.db.UpdateProcessNodeRuntimeOrders(node.ID, &order.ID, nil); err != nil {
		log.Printf("quality-hold: update runtime orders for node %d: %v", node.ID, err)
	}
	log.Printf("quality-hold: order %d carries bin %d (%s) from %s to containment %s (held by %s)",
		order.ID, bin.BinID, bin.PayloadCode, node.Name, containment, actor)
	return order, nil
}

// containmentNodeMatches reports whether a containment destination names the
// node a bin sits at: directly (a concrete hold spot), or as a MEMBER of the
// destination's node group (a group destination — the bins sit on the group's
// children, and a child's name may be the group-prefixed form Core mints, so
// the suffix matches too). A read error answers false, which routes to the
// caller's "no claim covers this" refusal — visible, not guessed.
func (e *Engine) containmentNodeMatches(dest, nodeName string) bool {
	if dest == "" || nodeName == "" {
		return false
	}
	if dest == nodeName {
		return true
	}
	children, err := e.coreClient.FetchNodeChildren(dest, false)
	if err != nil {
		return false
	}
	for _, ch := range children {
		if ch.Name == nodeName || strings.HasSuffix(ch.Name, "."+nodeName) {
			return true
		}
	}
	return false
}

// ReleaseFromContainment sends a verified bin from a containment node back to
// the claim's ordinary outbound — the FG drop. The claim lookup keys on the
// containment node's OWN claims: the node the bin sits at is the destination
// some claim's Quality Containment section named, and that same claim's
// outbound is where a verified bin goes.
func (e *Engine) ReleaseFromContainment(containmentNodeName string, binID int64, actor string) (*orders.Order, error) {
	if actor == "" {
		actor = "operator"
	}
	// The claims whose containment route COVERS this node. A destination may
	// be the node itself (a concrete hold spot) or a node GROUP — and a
	// group's bins sit on the group's CHILDREN, whose names Core mints
	// group-prefixed ("Quality Hold.PLK_X1"), so the match is membership,
	// not equality. This is what the first release attempt taught: a claim
	// pointing at the group and a tile named for its child never matched
	// verbatim, and the release refused everything.
	all, err := e.db.ListAllContainmentClaims()
	if err != nil {
		return nil, fmt.Errorf("read containment claims: %w", err)
	}
	var claims []processes.NodeClaim
	for _, c := range all {
		if e.containmentNodeMatches(c.ContainmentDestination, containmentNodeName) {
			claims = append(claims, c)
		}
	}

	// THE BIN, EARLY: a shared hold group is legitimate — several producers
	// route their contained bins to one spot with different FG drops — and
	// the BIN'S PAYLOAD is what disambiguates whose outbound a release uses.
	// So the bin is read before the claim set is narrowed, and only claims
	// whose payload set covers the carried payload survive it.
	binsAtNode, _, err := e.coreClient.FetchNodeBins([]string{containmentNodeName})
	if err != nil {
		return nil, fmt.Errorf("read bins at %s: %w", containmentNodeName, err)
	}
	found := false
	var payloadCode string
	for _, b := range binsAtNode {
		if b.Occupied && b.BinID == binID {
			found = true
			payloadCode = b.PayloadCode
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("bin %d is not at %s any more — refresh the screen", binID, containmentNodeName)
	}
	if payloadCode == "" {
		return nil, fmt.Errorf("bin %d carries no payload — a release needs the bin's payload to resolve whose outbound it goes to", binID)
	}
	var routed []processes.NodeClaim
	for _, c := range claims {
		if c.PayloadCode == payloadCode || slices.Contains(c.AllowedPayloads(), payloadCode) {
			routed = append(routed, c)
		}
	}
	if len(routed) == 0 {
		return nil, fmt.Errorf("no claim that produces %s routes through %s — the covering claims are for other payloads", payloadCode, containmentNodeName)
	}
	claims = routed

	outbounds := make([]string, 0, len(claims))
	for _, c := range claims {
		if c.OutboundDestination != "" && !slices.Contains(outbounds, c.OutboundDestination) {
			outbounds = append(outbounds, c.OutboundDestination)
		}
	}
	if len(outbounds) != 1 {
		return nil, fmt.Errorf("claims for %s on %s disagree about the outbound (%d distinct) — fix the claims before releasing",
			payloadCode, containmentNodeName, len(outbounds))
	}
	outbound := outbounds[0]

	// (The bin's presence was verified above — the stale-screen guard already
	// ran, before the claim narrowing, because the payload disambiguation
	// needs the bin's payload.)

	// The containment node's process node id (the move order is keyed on the
	// source process node, like every operator move).
	var nodeID int64
	if err := e.db.DB.QueryRow(`SELECT id FROM process_nodes WHERE core_node_name = ?`, containmentNodeName).Scan(&nodeID); err != nil {
		return nil, fmt.Errorf("resolve containment node %s: %w", containmentNodeName, err)
	}

	// The move FIRST, the hold clear after it — every refusal above precedes
	// every side effect, and the marker write is the release's audit tail, not
	// its gate. A failed clear leaves the marker on a bin that is leaving:
	// logged, and the bin's own arrival at the FG outbound supersedes the
	// stale chip on any screen that refreshes.
	order, err := e.orderMgr.CreateMoveOrderWithPayloadCode(&nodeID, 1, containmentNodeName, outbound, payloadCode, true,
		ordermgr.NoDemand())
	if err != nil {
		return nil, fmt.Errorf("create release move: %w", err)
	}
	if err := e.coreClient.SetBinQualityHold(binID, false, actor); err != nil {
		log.Printf("quality-hold: release move %d created but the hold clear failed for bin %d: %v", order.ID, binID, err)
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil); err != nil {
		log.Printf("quality-hold: update runtime orders for containment node %d: %v", nodeID, err)
	}
	log.Printf("quality-hold: release order %d carries bin %d (%s) from containment %s to %s (verified by %s)",
		order.ID, binID, payloadCode, containmentNodeName, outbound, actor)
	return order, nil
}

// RecallContainedPayload walks every bin of a contained payload that is still
// sitting at one of its claims' FG outbound nodes into that claim's
// containment destination. Returns the number of moves created and a list of
// per-bin refusals (each with its reason) — a recall that partially succeeds
// reports both halves, because "3 recalled, 2 refused: occupied" is the
// operator's sentence, not an error code.
func (e *Engine) RecallContainedPayload(payloadCode, actor string) (int, []string, error) {
	if actor == "" {
		actor = "operator"
	}
	claims, err := e.db.ListContainmentClaimsForPayload(payloadCode)
	if err != nil {
		return 0, nil, fmt.Errorf("read containment claims for %s: %w", payloadCode, err)
	}
	if len(claims) == 0 {
		return 0, nil, fmt.Errorf("no claim for payload %s declares a containment destination", payloadCode)
	}
	created := 0
	var refusals []string
	for _, claim := range claims {
		if claim.OutboundDestination == "" {
			continue
		}
		binsAtOutbound, _, err := e.coreClient.FetchNodeBins([]string{claim.OutboundDestination})
		if err != nil {
			refusals = append(refusals, fmt.Sprintf("%s: read failed: %v", claim.OutboundDestination, err))
			continue
		}
		for _, b := range binsAtOutbound {
			if !b.Occupied || b.PayloadCode != payloadCode {
				continue
			}
			var nodeID int64
			if qerr := e.db.DB.QueryRow(`SELECT id FROM process_nodes WHERE core_node_name = ?`, claim.OutboundDestination).Scan(&nodeID); qerr != nil {
				refusals = append(refusals, fmt.Sprintf("bin %d at %s: node is not a process node", b.BinID, claim.OutboundDestination))
				continue
			}
			// One move at a time per node (the double-tap guard): a bin whose
			// node already has an active move is either leaving or arriving.
			if existing, lerr := e.db.ListActiveOrdersByProcessNodeAndType(nodeID, protocol.OrderTypeMove); lerr != nil {
				refusals = append(refusals, fmt.Sprintf("bin %d at %s: in-flight check failed: %v", b.BinID, claim.OutboundDestination, lerr))
				continue
			} else if len(existing) > 0 {
				refusals = append(refusals, fmt.Sprintf("bin %d at %s: a move order (%d) is already in flight", b.BinID, claim.OutboundDestination, existing[0].ID))
				continue
			}
			order, merr := e.orderMgr.CreateMoveOrderWithPayloadCode(&nodeID, 1, claim.OutboundDestination, claim.ContainmentDestination, payloadCode, true,
				ordermgr.NoDemand())
			if merr != nil {
				refusals = append(refusals, fmt.Sprintf("bin %d at %s: %v", b.BinID, claim.OutboundDestination, merr))
				continue
			}
			if herr := e.coreClient.SetBinQualityHold(b.BinID, true, actor); herr != nil {
				log.Printf("quality-hold: recall move %d created but the hold marker failed for bin %d: %v", order.ID, b.BinID, herr)
			}
			if uerr := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil); uerr != nil {
				log.Printf("quality-hold: update runtime orders for node %d: %v", nodeID, uerr)
			}
			log.Printf("quality-hold: recall order %d carries bin %d (%s) from %s to containment %s (by %s)",
				order.ID, b.BinID, payloadCode, claim.OutboundDestination, claim.ContainmentDestination, actor)
			created++
		}
	}
	return created, refusals, nil
}
