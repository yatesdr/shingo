package engine

import (
	"fmt"
	"sort"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
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
//
// A CHANGEOVER DECIDES TWO THINGS AT A SPOT, AND REFILLS IT NEVER. Whether the
// spare standing there goes back (sendsBack), and whether each incoming leg
// takes it (legDiffs, through requestClaim, the REQUEST's own door). The refill
// is the keeper's, during the changeover as at any other time: it reads the
// spot for the incoming claim from START to cutover (keptFor) and orders behind
// the leg's lift, not ahead of it. The start once ordered refills itself, behind
// a lift it assumed, and at SPR ALN_011 (2026-10-15 08:31) the leg lifted the
// spare the start had judged wrong and sent back: a return with nothing to carry
// and two refills bound for a one-bin spot.

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
}

// changeoverSpots lists every keep-staged spot the outgoing or the incoming
// claims name, with who keeps it and who kept it. Pure. Called with the styles
// reversed to put a cancelled changeover's spots back.
func changeoverSpots(outgoing, incoming []processes.NodeClaim, nodes []processes.Node) []spotChange {
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
		if spot := spotNode(c); spot != "" {
			ch := at(spot)
			ch.leaver, ch.leaverNode = c, byNode[c.CoreNodeName]
		}
	}
	for i := range incoming {
		c := &incoming[i]
		if spot := spotNode(c); spot != "" {
			ch := at(spot)
			ch.keeper, ch.keeperNode = c, byNode[c.CoreNodeName]
		}
	}
	out := make([]spotChange, 0, len(changes))
	for _, ch := range changes {
		out = append(out, *ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].spot < out[j].spot })
	return out
}

// sendsBack is the changeover's decision for one spot, from Core's read with a
// spare already leaving taken off it (readsLessLeaving): the bin standing there
// goes back when no incoming claim keeps the spot, or it does not suit the
// incoming claim (spareIsRight): a full by its part, an empty by whether the
// incoming part may ride its carrier. The bin is judged by what it is, not by
// who left it there or when: an empty one style left serves the other when both
// parts ride its carrier, and at a cancel the empty standing is judged the same
// whichever style it came from. Nothing is decided on an unknown read; a start
// refuses one (unknownSpots).
func (ch spotChange) sendsBack(read spotRead) bool {
	if !read.known || !read.occupied {
		return false
	}
	return ch.keeper == nil || !spareIsRight(ch.keeper, read)
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

// spotReturns is the spots whose spare goes back, each decided once: the start
// and the cancel count them and create them from this one list.
func spotReturns(changes []spotChange, reads map[string]spotRead) []spotChange {
	var out []spotChange
	for _, ch := range changes {
		if ch.sendsBack(reads[ch.spot]) {
			out = append(out, ch)
		}
	}
	return out
}

// readsLessLeaving takes a spare that already has a live return off its spot's
// read (lessLeaving), from the live rows the decision point has read, leaving
// out the orders it has just aborted. A return is a plain move off the spot to
// anywhere but a line that keeps or kept it.
func readsLessLeaving(reads map[string]spotRead, live []domain.Order, changes []spotChange, aborted map[int64]bool) map[string]spotRead {
	out := make(map[string]spotRead, len(reads))
	for spot, r := range reads {
		out[spot] = r
	}
	for _, ch := range changes {
		lines := map[string]bool{}
		for _, c := range []*processes.NodeClaim{ch.keeper, ch.leaver} {
			if c != nil {
				lines[c.CoreNodeName] = true
			}
		}
		leaving := 0
		for i := range live {
			o := &live[i]
			if aborted[o.ID] || ordermgr.IsTerminal(o.Status) {
				continue
			}
			if o.OrderType == protocol.OrderTypeMove && o.SourceNode == ch.spot && !lines[o.DeliveryNode] {
				leaving++
			}
		}
		if r, ok := out[ch.spot]; ok {
			out[ch.spot] = r.lessLeaving(leaving)
		}
	}
	return out
}

// legDiffs is the diffs the changeover's legs are built from: the supply of
// each incoming claim that keeps a spot decided by requestClaim, the REQUEST's
// door, from the same read. A leg takes the spare standing there when it is
// right and not leaving, and is otherwise the market swap a line with no spot
// runs (withoutSpot); the keeper fills the spot behind it.
//
// A copy: plan.diffs and plan.toClaims keep the claim as saved, which is what
// the node tasks record and StageNodeChangeoverMaterial's refusal reads. An Add
// is left as it is: its carrier goes straight to the line and never touches the
// spot (planAddAction).
func legDiffs(diffs []ChangeoverNodeDiff, reads map[string]spotRead) []ChangeoverNodeDiff {
	out := make([]ChangeoverNodeDiff, len(diffs))
	copy(out, diffs)
	for i := range out {
		d := &out[i]
		spot := spotNode(d.ToClaim)
		if spot == "" || d.Situation == SituationAdd {
			continue
		}
		d.ToClaim, _ = requestClaim(d.ToClaim, d.ToClaim, spotAtRequest{read: reads[spot]})
	}
	return out
}

// unknownSpots names every spot Core did not answer for, sorted.
func unknownSpots(changes []spotChange, reads map[string]spotRead) []string {
	var out []string
	for _, ch := range changes {
		if !reads[ch.spot].known {
			out = append(out, ch.spot)
		}
	}
	return out
}

// rearmKeptLines is a START re-arming the keeper on every line the incoming
// style keeps a spot for, after the start's aborts: pressing START is a person
// saying go, as REQUEST and the board's RESUME are (rearmKeepStaged). Without
// it, a start that aborted a queued keeper refill leaves the keeper paused: the
// legs are written in the abort's own second, and spotPause re-arms only on an
// order strictly newer than the abort.
func (e *Engine) rearmKeptLines(claims []processes.NodeClaim, nodes []processes.Node) {
	for i := range claims {
		if node := findNodeByCoreName(nodes, claims[i].CoreNodeName); node != nil {
			e.rearmKeepStaged(node, &claims[i])
		}
	}
}

// errUnknownSpots is the refusal a changeover's start and preview give when Core
// did not say what stands on a keep-staged spot: the leg cannot be decided, as a
// REQUEST's cannot (readSpotAtRequest fails closed), and the next press re-asks.
func errUnknownSpots(spots []string) error {
	return fmt.Errorf("cannot start changeover — Core did not say what stands on keep-staged spot %s; press again",
		strings.Join(spots, ", "))
}

// lockKeepStagedCells takes the prime lock of every line in lines, in name
// order, and returns their release. A changeover start or cancel holds them
// from its read of the spots to its last spot order, as a request holds its
// cell's, so neither decides a spot from rows the other is about to write.
func (e *Engine) lockKeepStagedCells(lines []string) func() {
	sorted := append([]string(nil), lines...)
	sort.Strings(sorted)
	var held []cellLock
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
func keepStagedLines(claimSets ...[]processes.NodeClaim) []string {
	var out []string
	for _, claims := range claimSets {
		for i := range claims {
			if spotNode(&claims[i]) != "" {
				out = append(out, claims[i].CoreNodeName)
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
func keepStagedSpotNames(claimSets ...[]processes.NodeClaim) []string {
	seen := map[string]bool{}
	var out []string
	for _, claims := range claimSets {
		for i := range claims {
			if spot := spotNode(&claims[i]); spot != "" && !seen[spot] {
				seen[spot] = true
				out = append(out, spot)
			}
		}
	}
	sort.Strings(out)
	return out
}

// readSpots asks Core about every spot in ONE call. A spot Core does not
// answer for, or does not have, reads unknown.
func (e *Engine) readSpots(changes []spotChange) map[string]spotRead {
	reads, _ := e.readSpotsAnd(changes, nil)
	return reads
}

// readSpotsAnd is readSpots with more nodes in the same call: also's rows come
// back whole, by node name. Nothing to ask about, no call.
func (e *Engine) readSpotsAnd(changes []spotChange, also []string) (map[string]spotRead, map[string]NodeBinInfo) {
	reads := make(map[string]spotRead, len(changes))
	extra := map[string]NodeBinInfo{}
	if len(changes)+len(also) == 0 || e.coreClient == nil || !e.coreClient.Available() {
		return reads, extra
	}
	names := make([]string, 0, len(changes)+len(also))
	isSpot := make(map[string]bool, len(changes))
	for _, ch := range changes {
		names = append(names, ch.spot)
		isSpot[ch.spot] = true
	}
	isAlso := make(map[string]bool, len(also))
	for _, n := range also {
		isAlso[n] = true
		if !isSpot[n] {
			names = append(names, n)
		}
	}
	rows, _, err := e.coreClient.FetchNodeBins(names)
	if err != nil {
		e.logFn("keep-staged: read %d node(s) at changeover: %v — no spot orders this time", len(names), err)
		return reads, extra
	}
	catalog := e.PayloadBinTypes()
	for _, b := range rows {
		if isSpot[b.NodeName] && e.spotNodeKnown(b.NodeName) {
			reads[b.NodeName] = readOfRow(b, catalog)
		}
		if isAlso[b.NodeName] {
			extra[b.NodeName] = b
		}
	}
	return reads, extra
}

// applyChangeoverSpots sends back each spare the decision sent back
// (spotReturns). The returns carry the changeover's origin and no task link (a
// task-linked return would be read as the changeover's own material).
func (e *Engine) applyChangeoverSpots(returns []spotChange, reads map[string]spotRead, origin ordermgr.Origin) {
	for _, ch := range returns {
		read := reads[ch.spot]
		if c, n := ch.returnTo(); c != nil && n != nil {
			e.returnSpare(n, ch.spot, c.InboundSource, read.payload, read.binID, origin)
			e.logFn("keep-staged: changeover spot %s: the spare goes back to %s", ch.spot, c.InboundSource)
		}
	}
}

// cancelledChangeoverSpots is changeoverSpots for a changeover being cancelled:
// the incoming style's claims are the ones leaving, the outgoing style's the
// ones keeping.
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
	return changeoverSpots(leaving, staying, nodes)
}

// abortSpotOrdersNotFlown cancels every plain order to or from a spot that is
// not yet with the fleet: refills bound for it and returns leaving it. A refill
// was the keeper's for the incoming style, and left alive it would land that
// style's spare; a return was the start's, and would carry away a spare the
// style that stays may want. An order already flown lands: a refill the cancel
// could not stop goes back when it does (returnUnwantedLanding), and a return
// with the fleet counts as the spare leaving. It returns the live rows it read
// and the ids it aborted, for readsLessLeaving.
func (e *Engine) abortSpotOrdersNotFlown(spots []spotChange) ([]domain.Order, map[int64]bool) {
	if len(spots) == 0 {
		return nil, nil
	}
	isSpot := make(map[string]bool, len(spots))
	for _, ch := range spots {
		isSpot[ch.spot] = true
	}
	live, err := e.db.ListActiveOrders()
	if err != nil {
		e.logFn("keep-staged: list orders to cancel at the spots: %v", err)
		return nil, nil
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
	return live, aborted
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
			if spotNode(&c) == k.Spot {
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
				read = readOfRow(b, nil) // target zero: whatever stands there goes, so nothing is judged
			}
		}
		if !read.known || !read.occupied {
			continue
		}
		mu := e.primeNodeLock(&processes.NodeClaim{CoreNodeName: g.k.Line})
		mu.Lock()
		e.returnSpare(g.node, g.k.Spot, g.k.Source, read.payload, read.binID, ordermgr.NoDemand())
		mu.Unlock()
		e.logFn("keep-staged: spot %s no longer kept by %s: its spare goes back to %s", g.k.Spot, g.k.Line, g.k.Source)
	}
}
