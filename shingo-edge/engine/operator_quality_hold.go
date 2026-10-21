package engine

// operator_quality_hold.go — the operator side of quality containment (v100).
//
// Three verbs, all riding machinery that already exists:
//
//	SendBinToQualityHold   — the station's "Send to Quality Hold": the bin
//	                       standing at a produce node goes to the claim's
//	                       containment destination, NOW, as an explicit move
//	                       order naming the bin. The move IS the containment;
//	                       the hold marker (the audit and the re-release
//	                       protection) is asked for with it, and Core checks
//	                       the bin stands there, sets the marker and births
//	                       the move as one act (OrderRequest.QualityHoldBy).
//	                       A curtained node's hold is a complex order, which
//	                       carries no bin: there the marker is still the
//	                       separate best-effort call.
//	ReleaseFromContainment — the containment screen's "Verify Good": a
//	                       verified bin walks from the containment node to
//	                       the claim's ordinary outbound (the FG drop). The
//	                       hold marker clears with it.
//	RecallContainedPayload — the containment screen's recall: bins of a
//	                       contained payload still sitting at their claims'
//	                       FG outbound nodes get walked into containment, each
//	                       move naming its bin and asking for its hold.
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
	"shingoedge/release"
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

	// The bin standing at the node, from the bins table the station screen
	// draws (site 8 of the reads), under the decision rule: a table that may
	// not be decided from refuses with its sentence. No bin, nothing to hold.
	bin, err := e.binsRowAt(node.CoreNodeName)
	if err != nil {
		return nil, err
	}
	if !bin.Occupied {
		return nil, fmt.Errorf("no bin at node %s to hold", node.Name)
	}

	// Double-tap guard (the U2 empty-out guard's shape): the order layer has
	// no dedup for move orders, so a second tap while one is in flight would
	// send two robots for one carrier.
	if existing, lerr := e.db.ListActiveOrdersByProcessNodeAndType(nodeID, protocol.OrderTypeMove); lerr != nil {
		return nil, fmt.Errorf("check in-flight move: %w", lerr)
	} else if len(existing) > 0 {
		return nil, fmt.Errorf("a move order (%d) is already in flight for node %s", existing[0].ID, node.Name)
	}
	// On a curtained node the hold is a complex order (below), so the guard
	// covers that shape too: one headed to the same containment destination.
	if existing, lerr := e.db.ListActiveOrdersByProcessNodeAndType(nodeID, protocol.OrderTypeComplex); lerr != nil {
		return nil, fmt.Errorf("check in-flight hold: %w", lerr)
	} else {
		for _, o := range existing {
			if o.DeliveryNode == containment {
				return nil, fmt.Errorf("a quality hold (order %d) is already in flight for node %s", o.ID, node.Name)
			}
		}
	}

	nodeIDCopy := node.ID
	var order *orders.Order
	if curtained := e.curtainedCoreNodes(); curtained[node.CoreNodeName] {
		// A bin lifted off a curtained node gets a station wait in front of the
		// pickup (S7), so the order takes the complex shape: the same pickup and
		// drop, the same payload and auto-confirm. The press of QUALITY HOLD is the
		// operator's, remembered at the wait until the curtain clears.
		steps, firstWait := withCurtainWaits([]protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: node.CoreNodeName},
			{Action: protocol.ActionDropoff, Node: containment},
		}, curtained, func(n string) protocol.ComplexOrderStep { return stationWait(n, release.PurposeSwap) })
		order, err = e.orderMgr.CreateComplexOrderWithPayload(&nodeIDCopy, 1, containment, node.CoreNodeName, steps, true,
			bin.PayloadCode, ordermgr.NoDemand())
		if err != nil {
			return nil, fmt.Errorf("create containment move: %w", err)
		}
		e.writeCreationIntent(order.ID, firstWait, release.PurposeSwap, actor, release.Choices{})
		// The complex order carries no bin, so Core cannot hold one with it: the
		// marker stays the separate best-effort call. The move IS the
		// containment, so a failed marker write logs and continues rather than
		// telling an operator their hold failed when the robot is already coming.
		if err := e.SetBinQualityHold(bin.BinID, true, actor); err != nil {
			log.Printf("quality-hold: move %d created but the Core hold marker failed for bin %d: %v", order.ID, bin.BinID, err)
		}
	} else {
		// THE HOLD IS ONE CORE ACT: the move names the bin read above and asks
		// for its hold, and Core refuses the move unless that bin stands on the
		// node, setting the marker in the move's birth.
		order, err = e.orderMgr.CreateHoldMoveOrder(&nodeIDCopy, node.CoreNodeName, containment, bin.PayloadCode, bin.BinID, actor,
			ordermgr.NoDemand())
		if err != nil {
			return nil, fmt.Errorf("create containment move: %w", err)
		}
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
// the suffix matches too). The members are the containment copy's: Core sends
// every containment destination with its group children on the containment
// feed (store ListContainmentDestinations). No copy held, or a destination the
// copy does not name, answers false, which routes to the caller's "no claim
// covers this" refusal — visible, not guessed.
func (e *Engine) containmentNodeMatches(dest, nodeName string) bool {
	if dest == "" || nodeName == "" {
		return false
	}
	if dest == nodeName {
		return true
	}
	st, held := e.LocalContainment()
	if !held {
		return false
	}
	var children []string
	for _, d := range st.Destinations {
		if d.Node == dest {
			children = d.Children
		}
	}
	for _, ch := range children {
		if ch == nodeName || strings.HasSuffix(ch, "."+nodeName) {
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
	// whose payload set covers the carried payload survive it. The bins at a
	// containment node are the containment copy's (every destination with
	// what stands on it and on its children), under the decision rule: a copy
	// that may not be decided from refuses with its sentence.
	snap, err := e.containmentForDecision(containmentNodeName)
	if err != nil {
		return nil, err
	}
	found := false
	var payloadCode string
	for _, d := range snap.Destinations {
		for _, b := range d.Bins {
			if b.Node == containmentNodeName && b.BinID == binID {
				found = true
				payloadCode = b.PayloadCode
			}
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
	if err := e.SetBinQualityHold(binID, false, actor); err != nil {
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
		// The bin at the FG outbound, from its row in the bins table (site 10
		// of the reads; the produce claims' outbound destinations are on the
		// watch list). A table that cannot answer for it is that claim's
		// refusal, with its sentence; the other claims still recall.
		b, err := e.binsRowAt(claim.OutboundDestination)
		if err != nil {
			refusals = append(refusals, err.Error())
			continue
		}
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
		// One Core act, as the station's hold: the move names the bin read
		// above and asks for its hold (CreateHoldMoveOrder).
		order, merr := e.orderMgr.CreateHoldMoveOrder(&nodeID, claim.OutboundDestination, claim.ContainmentDestination,
			payloadCode, b.BinID, actor, ordermgr.NoDemand())
		if merr != nil {
			refusals = append(refusals, fmt.Sprintf("bin %d at %s: %v", b.BinID, claim.OutboundDestination, merr))
			continue
		}
		if uerr := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &order.ID, nil); uerr != nil {
			log.Printf("quality-hold: update runtime orders for node %d: %v", nodeID, uerr)
		}
		log.Printf("quality-hold: recall order %d carries bin %d (%s) from %s to containment %s (by %s)",
			order.ID, b.BinID, payloadCode, claim.OutboundDestination, claim.ContainmentDestination, actor)
		created++
	}
	return created, refusals, nil
}

// SetBinQualityHold sets or clears a bin's quality-hold marker on Core: an ask
// (bin.quality_hold). ErrAskPending when Core has not answered within
// AskWait; the marker is then Core's to set when the ask arrives, and the
// containment feed shows it.
func (e *Engine) SetBinQualityHold(binID int64, hold bool, by string) error {
	req := map[string]any{"bin_id": binID, "hold": hold, "by": by}
	_, err := askOrRefuse[struct{}](e, protocol.AskQualityHold, req)
	return err
}
