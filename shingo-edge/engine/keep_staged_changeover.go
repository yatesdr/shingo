package engine

import (
	"sort"
	"sync"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/engine/changeover"
	ordermgr "shingoedge/orders"
	"shingoedge/store/processes"
)

// keep_staged_changeover.go — keep-staged spots across a changeover.
//
// A changeover decides each spot ONCE, across the whole plan, not per line node:
// two line nodes in two styles of one process may name the same spot (config
// allows a spot to be reused across the styles of one process), and the spare
// one style leaves there may be exactly what the other wants. Deciding per line
// node, the outgoing claim's view would send away a spare the incoming one was
// about to use.

// spotChange is one spot through a changeover.
type spotChange struct {
	spot string
	// keeper is the incoming claim that keeps a spare on the spot; nil when no
	// incoming claim does, and then the spot is to be left empty.
	keeper     *processes.NodeClaim
	keeperNode *processes.Node
	// leaver is the outgoing claim that kept a spare there; nil when none did.
	leaver     *processes.NodeClaim
	leaverNode *processes.Node
	// consumes is 1 when the changeover's own supply leg lifts the spare.
	consumes int
}

// changeoverSpots lists every keep-staged spot the outgoing or the incoming
// claims name, with who keeps it, who kept it and whether the changeover's plan
// lifts it. Pure. Called with the styles reversed to put a cancelled
// changeover's spots back (an empty plan: nothing lifts them then).
func changeoverSpots(outgoing, incoming []processes.NodeClaim, nodes []processes.Node, plan changeover.Plan) []spotChange {
	byNode := make(map[string]*processes.Node, len(nodes))
	for i := range nodes {
		byNode[nodes[i].CoreNodeName] = &nodes[i]
	}
	changes := map[string]*spotChange{}
	at := func(spot string) *spotChange {
		if c := changes[spot]; c != nil {
			return c
		}
		c := &spotChange{spot: spot}
		changes[spot] = c
		return c
	}
	for i := range outgoing {
		c := &outgoing[i]
		if c.KeepStaged && c.InboundStaging != "" {
			ch := at(c.InboundStaging)
			ch.leaver, ch.leaverNode = c, byNode[c.CoreNodeName]
		}
	}
	for i := range incoming {
		c := &incoming[i]
		if c.KeepStaged && c.InboundStaging != "" {
			ch := at(c.InboundStaging)
			ch.keeper, ch.keeperNode = c, byNode[c.CoreNodeName]
		}
	}
	out := make([]spotChange, 0, len(changes))
	for _, ch := range changes {
		if ch.keeper != nil && planLiftsSpot(plan, ch.keeper.CoreNodeName, ch.spot) {
			ch.consumes = 1
		}
		out = append(out, *ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].spot < out[j].spot })
	return out
}

// planLiftsSpot reports whether the plan's action for a line node picks up at
// the spot: the short supply a keep-staged incoming claim gets (changeoverDispatch).
func planLiftsSpot(plan changeover.Plan, coreNode, spot string) bool {
	for _, a := range plan.Actions {
		if a.CoreNodeName != coreNode || a.Err != nil {
			continue
		}
		for _, spec := range []*changeover.OrderSpec{a.SupplyOrder, a.EvacOrder} {
			for _, s := range specSteps(spec) {
				if s.Action == protocol.ActionPickup && s.Node == spot {
					return true
				}
			}
		}
	}
	return false
}

// decide is the reconcile for one spot through a changeover, from Core's read.
//
// A spare is unwanted when no incoming claim keeps the spot, or it does not suit
// the incoming claim. A full spare names its part, so the read decides
// (spareIsRight). An empty does not, and Edge holds no carrier rule: for a
// produce keeper, an empty is judged by config, kept only when the outgoing
// claim was produce for the same part. That clause sends an empty back even
// when its carrier would have served; Core judges type at the pickup, and a
// wrong one would hold the supply. It must not judge a full spare: after a
// cancel, the spare standing is often the staying style's own, which never
// left, and the config would read it as the incoming style's. Nor an empty the
// staying style is known to own (flow.keeperOwns): at a cancel, unless one of
// the incoming style's refills has landed since the start, the empty standing
// is the staying style's, and judged by config it went back to the incoming
// style's source.
//
// What is already with the fleet is counted (flow): start and cancel abort
// every spot order not yet with it, but one that has flown lands. A refill on
// its way for the keeper's part counts as coming. A spare with a live return
// is already going, so the spot is decided as bare: kept as standing, nothing
// would be ordered behind it and the return would leave the spot empty; judged
// again, it would be sent back twice.
func (ch spotChange) decide(read spotRead, flow spotFlow) spotPlan {
	if !read.known {
		return spotPlan{}
	}
	read = read.lessLeaving(flow.leaving)
	judge := ch.keeper
	target := 1
	if judge == nil {
		judge, target = ch.leaver, 0
	}
	right := ch.keeper != nil && spareIsRight(ch.keeper, read)
	if right && ch.keeper.Role == protocol.ClaimRoleProduce && ch.leaver != nil && !flow.keeperOwns &&
		(ch.leaver.Role != ch.keeper.Role || ch.leaver.PayloadCode != ch.keeper.PayloadCode) {
		right = false
	}
	return reconcileSpot(judge, read.occupied, right, flow.coming, ch.consumes, target)
}

// returnTo is where an unwanted spare goes: back to whatever supplied it, the
// outgoing claim's inbound source, or the incoming claim's when no outgoing
// claim kept the spot.
func (ch spotChange) returnTo() (*processes.NodeClaim, *processes.Node) {
	if ch.leaver != nil {
		return ch.leaver, ch.leaverNode
	}
	return ch.keeper, ch.keeperNode
}

// spotFlow is what is already moving at a spot when a changeover decides it:
// refills on their way for the keeper's part and role, and live returns off it.
// keeperOwns says the bin standing there is known to be the keeper's own: at a
// cancel, when none of the leaving style's refills has landed since the
// changeover started, whatever stands there was never the leaver's.
type spotFlow struct {
	coming, leaving int
	keeperOwns      bool
}

// spotFlows counts each spot's flow from the live rows the decision point has
// already read, leaving out the orders it has just aborted. landedSince is set
// at a cancel: the changeover's start, after which a confirmed refill of the
// leaving claim's part and role is that style's bin on the spot.
func spotFlows(live []domain.Order, changes []spotChange, aborted map[int64]bool, landedSince *time.Time) map[string]spotFlow {
	flows := make(map[string]spotFlow, len(changes))
	for _, ch := range changes {
		lines := map[string]bool{}
		for _, c := range []*processes.NodeClaim{ch.keeper, ch.leaver} {
			if c != nil {
				lines[c.CoreNodeName] = true
			}
		}
		var f spotFlow
		leaverLanded := false
		for i := range live {
			o := &live[i]
			if landedSince != nil && ch.leaver != nil && o.Status == ordermgr.StatusConfirmed &&
				isRetrieve(o.OrderType) && o.DeliveryNode == ch.spot && o.PayloadCode == ch.leaver.PayloadCode &&
				o.RetrieveEmpty == (ch.leaver.Role == protocol.ClaimRoleProduce) && !o.CreatedAt.Before(*landedSince) {
				leaverLanded = true
			}
			if aborted[o.ID] || ordermgr.IsTerminal(o.Status) {
				continue
			}
			switch {
			case o.OrderType == protocol.OrderTypeMove && o.SourceNode == ch.spot && !lines[o.DeliveryNode]:
				f.leaving++
			case ch.keeper != nil && isSpotRefill(o, ch.keeper) && o.PayloadCode == ch.keeper.PayloadCode &&
				o.RetrieveEmpty == (ch.keeper.Role == protocol.ClaimRoleProduce):
				f.coming++
			}
		}
		f.keeperOwns = landedSince != nil && !leaverLanded
		flows[ch.spot] = f
	}
	return flows
}

// lockKeepStagedCells takes the prime lock of every line in lines, in name
// order, and returns their release. A changeover start or cancel holds them
// from its read of the spots to its last spot order, as a request holds its
// cell's, so neither decides a spot from rows the other is about to write.
func (e *Engine) lockKeepStagedCells(lines []string) func() {
	sorted := append([]string(nil), lines...)
	sort.Strings(sorted)
	var held []*sync.Mutex
	for i, line := range sorted {
		if i > 0 && line == sorted[i-1] {
			continue
		}
		mu := e.primeNodeLock(&processes.NodeClaim{CoreNodeName: line})
		mu.Lock()
		held = append(held, mu)
	}
	return func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i].Unlock()
		}
	}
}

// keepStagedLines is every line node whose outgoing or incoming claim keeps a
// spot.
func keepStagedLines(diffs []ChangeoverNodeDiff) []string {
	var out []string
	for _, d := range diffs {
		for _, c := range []*processes.NodeClaim{d.FromClaim, d.ToClaim} {
			if c != nil && c.KeepStaged && c.InboundStaging != "" {
				out = append(out, c.CoreNodeName)
			}
		}
	}
	return out
}

// spotLines is every line node that keeps or kept one of the spots.
func spotLines(changes []spotChange) []string {
	var out []string
	for _, ch := range changes {
		for _, c := range []*processes.NodeClaim{ch.keeper, ch.leaver} {
			if c != nil {
				out = append(out, c.CoreNodeName)
			}
		}
	}
	return out
}

// keepStagedSpotNames is every keep-staged spot either style names: the nodes
// the changeover start gate covers and the cancel aborts toward.
func keepStagedSpotNames(diffs []ChangeoverNodeDiff) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range diffs {
		for _, c := range []*processes.NodeClaim{d.FromClaim, d.ToClaim} {
			if c != nil && c.KeepStaged && c.InboundStaging != "" && !seen[c.InboundStaging] {
				seen[c.InboundStaging] = true
				out = append(out, c.InboundStaging)
			}
		}
	}
	sort.Strings(out)
	return out
}

// readSpots asks Core about every spot in ONE call. A spot Core does not
// answer for, or does not have, reads unknown and gets nothing.
func (e *Engine) readSpots(changes []spotChange) map[string]spotRead {
	reads := make(map[string]spotRead, len(changes))
	if len(changes) == 0 || e.coreClient == nil || !e.coreClient.Available() {
		return reads
	}
	names := make([]string, 0, len(changes))
	for _, ch := range changes {
		names = append(names, ch.spot)
	}
	rows, _, err := e.coreClient.FetchNodeBins(names)
	if err != nil {
		e.logFn("keep-staged: read %d spot(s) at changeover: %v — no spot orders this time", len(names), err)
		return reads
	}
	for _, b := range rows {
		if e.spotNodeKnown(b.NodeName) {
			reads[b.NodeName] = spotRead{known: true, occupied: b.Occupied, payload: b.PayloadCode, bare: b.Bare}
		}
	}
	return reads
}

// spotOrderCount is how many orders the spot decisions create, for the episode's
// expected count.
func spotOrderCount(changes []spotChange, reads map[string]spotRead, flows map[string]spotFlow) int {
	n := 0
	for _, ch := range changes {
		n += ch.decide(reads[ch.spot], flows[ch.spot]).orders()
	}
	return n
}

// applyChangeoverSpots creates the spot orders: each wrong or unwanted spare's
// return, then the refills. They carry the changeover's origin and no task link
// (a task-linked refill would be read as the changeover's own next material).
func (e *Engine) applyChangeoverSpots(changes []spotChange, reads map[string]spotRead, flows map[string]spotFlow, origin ordermgr.Origin) {
	for _, ch := range changes {
		read := reads[ch.spot]
		plan := ch.decide(read, flows[ch.spot])
		if plan.returnSpare {
			if c, n := ch.returnTo(); c != nil && n != nil {
				e.returnSpare(n, ch.spot, c.InboundSource, read.payload, origin)
			}
		}
		if plan.refills > 0 && ch.keeper != nil && ch.keeperNode != nil {
			e.refillSpot(ch.keeperNode, ch.keeper, plan.refills, origin)
		}
		if plan.orders() > 0 {
			e.logFn("keep-staged: changeover spot %s: return=%v refills=%d", ch.spot, plan.returnSpare, plan.refills)
		}
	}
}

// claimsOfDiffs splits a changeover's diffs into the outgoing and incoming
// claims they carry.
func claimsOfDiffs(diffs []ChangeoverNodeDiff) (outgoing, incoming []processes.NodeClaim) {
	for _, d := range diffs {
		if d.FromClaim != nil {
			outgoing = append(outgoing, *d.FromClaim)
		}
		if d.ToClaim != nil {
			incoming = append(incoming, *d.ToClaim)
		}
	}
	return outgoing, incoming
}

// cancelledChangeoverSpots is changeoverSpots for a changeover being cancelled:
// the incoming style's claims are the ones leaving, the outgoing style's the
// ones keeping, and no plan lifts anything.
func (e *Engine) cancelledChangeoverSpots(processID int64, co *processes.Changeover) []spotChange {
	var staying, leaving []processes.NodeClaim
	if co.FromStyleID != nil {
		claims, err := e.db.ListStyleNodeClaims(*co.FromStyleID)
		if err != nil {
			e.logFn("keep-staged: changeover %d cancel: read the outgoing style's claims: %v — spots left as they are", co.ID, err)
			return nil
		}
		staying = claims
	}
	claims, err := e.db.ListStyleNodeClaims(co.ToStyleID)
	if err != nil {
		e.logFn("keep-staged: changeover %d cancel: read the incoming style's claims: %v — spots left as they are", co.ID, err)
		return nil
	}
	leaving = claims
	nodes, err := e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		e.logFn("keep-staged: changeover %d cancel: read the process's nodes: %v — spots left as they are", co.ID, err)
		return nil
	}
	return changeoverSpots(leaving, staying, nodes, changeover.Plan{})
}

// abortSpotOrdersNotFlown cancels every plain order to or from a spot that is
// not yet with the fleet: refills bound for it and returns leaving it. They were
// made for the changeover being cancelled, and left alive they would land the
// incoming style's spare or carry away the outgoing style's. An order already
// flown lands; the reconcile after it, or the next one, sends back what is wrong.
func (e *Engine) abortSpotOrdersNotFlown(spots []spotChange, startedAt time.Time) map[string]spotFlow {
	if len(spots) == 0 {
		return nil
	}
	isSpot := make(map[string]bool, len(spots))
	for _, ch := range spots {
		isSpot[ch.spot] = true
	}
	live, err := e.db.ListActiveOrders()
	if err != nil {
		e.logFn("keep-staged: list orders to cancel at the spots: %v", err)
		return nil
	}
	aborted := map[int64]bool{}
	for _, o := range live {
		if !isRetrieve(o.OrderType) && o.OrderType != protocol.OrderTypeMove {
			continue
		}
		if !isSpot[o.DeliveryNode] && !isSpot[o.SourceNode] {
			continue
		}
		if protocol.ChangeoverStartActionFor(o.Status) != protocol.ChangeoverStartCancel {
			continue
		}
		if err := e.orderMgr.AbortOrderWithReason(o.ID, "changeover cancelled: the keep-staged spot goes back to the outgoing style"); err != nil {
			e.logFn("keep-staged: cancel spot order %d: %v", o.ID, err)
			continue
		}
		aborted[o.ID] = true
	}
	return spotFlows(live, spots, aborted, &startedAt)
}

// spotsCleared is the save that clears a keep-staged flag, moves a spot or
// drops a keep-staged claim: target zero for each spot it left. A spare left
// standing there would hold the full shape's staging dropoff behind it, so it
// goes back to the claim's inbound source by a plain move, as the request path
// sends back a wrong one.
//
// Only for the RUNNING style. A style that is not running has no spare on the
// spot: whatever stands there is the running style's. And not for a spot the
// running style still keeps after the save — a spot moved from one cell to
// another in one save is the new keeper's, and its own request judges the
// spare there. One node-bins read for every spot the save left.
func (e *Engine) spotsCleared(moved []processes.KeptSpot) {
	if len(moved) == 0 {
		return
	}
	type gone struct {
		k    processes.KeptSpot
		node *processes.Node
	}
	var left []gone
	for _, k := range moved {
		style, err := e.db.GetStyle(k.StyleID)
		if err != nil || style == nil {
			continue
		}
		process, err := e.db.GetProcess(style.ProcessID)
		if err != nil || process == nil || process.ActiveStyleID == nil || *process.ActiveStyleID != k.StyleID {
			continue
		}
		claims, err := e.db.ListStyleNodeClaims(k.StyleID)
		if err != nil {
			e.logFn("keep-staged: spot %s left by %s: read the running style: %v — spare left where it stands", k.Spot, k.Line, err)
			continue
		}
		stillKept := false
		for _, c := range claims {
			if c.KeepStaged && c.InboundStaging == k.Spot {
				stillKept = true
			}
		}
		if stillKept {
			continue
		}
		nodes, err := e.db.ListProcessNodesByProcess(style.ProcessID)
		node := findNodeByCoreName(nodes, k.Line)
		if err != nil || node == nil {
			e.logFn("keep-staged: spot %s left by %s: no process node for the line — spare left where it stands", k.Spot, k.Line)
			continue
		}
		left = append(left, gone{k, node})
	}
	if len(left) == 0 || e.coreClient == nil || !e.coreClient.Available() {
		return
	}
	names := make([]string, 0, len(left))
	for _, g := range left {
		names = append(names, g.k.Spot)
	}
	rows, _, err := e.coreClient.FetchNodeBins(names)
	if err != nil {
		e.logFn("keep-staged: read %d spot(s) left by a save: %v — spares left where they stand", len(names), err)
		return
	}
	for _, g := range left {
		read := spotRead{}
		for _, b := range rows {
			if b.NodeName == g.k.Spot && e.spotNodeKnown(b.NodeName) {
				read = spotRead{known: true, occupied: b.Occupied, payload: b.PayloadCode, bare: b.Bare}
			}
		}
		if !read.known || !read.occupied {
			continue
		}
		mu := e.primeNodeLock(&processes.NodeClaim{CoreNodeName: g.k.Line})
		mu.Lock()
		e.returnSpare(g.node, g.k.Spot, g.k.Source, read.payload, ordermgr.NoDemand())
		mu.Unlock()
		e.logFn("keep-staged: spot %s no longer kept by %s: its spare goes back to %s", g.k.Spot, g.k.Line, g.k.Source)
	}
}
