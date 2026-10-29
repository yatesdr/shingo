package service

import (
	"log"
	"slices"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/lineside"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// servedRows is the board's served pass: what the board's loaders serve, one
// row per served cell's claim and part. It replaces the per-tile lineside
// fields (the parts a running style uses, their lineside, the changeover
// directive); the board derives those from the rows (operator-board-model.js).
//
// Run only when the board has a loader tile, and nil (absent on the wire) when
// it has none or a read fails: a board that could not look must not read as a
// board that looked and found nothing ([]).
//
// THE ROWS ARE OF THE BOARD'S ROLE. A loader (produce) fills cells that consume
// its parts; an unloader (consume) empties cells that produce them. Consume rows
// on a loader, produce rows on an unloader, both on a station with both.
//
// FIVE STATEMENTS, whatever the process count: the running and incoming styles'
// claims with their processes (one join), the plant's nodes, the served nodes'
// runtimes, their active piles, and their queued orders (the cells that have
// asked). While a served cell has an incoming style, one more: Core's sourcing
// verdicts. Pinned by TestServedRows_StatementBudget.
//
// A stage-2 tile's bare-cart card (stampStageTwoCard) comes from the same
// claims read: no statement more, and no read of Core.
func (s *StationService) servedRows(tiles []store.StationNodeView) []domain.ServedRow {
	parts, roles := boardServes(tiles)
	if len(roles) == 0 {
		return nil
	}
	claims, err := processes.ListServedClaims(s.db.DB)
	if err != nil {
		log.Printf("station view: served pass: list claims: %v", err)
		return nil
	}
	claims = slices.DeleteFunc(claims, func(c processes.ServedClaim) bool { return s.isLoaderClaim(c.Claim) })
	s.stampStageTwoCard(tiles, claims)
	if len(claims) == 0 {
		return []domain.ServedRow{}
	}
	nodes, err := processes.ListNodes(s.db.DB)
	if err != nil {
		log.Printf("station view: served pass: list nodes: %v", err)
		return nil
	}
	rows, styleOf := s.rowsOf(claims, nodes, parts, roles)
	if len(rows) == 0 {
		return rows
	}
	nodeIDs := make([]int64, 0, len(rows))
	for _, r := range rows {
		nodeIDs = append(nodeIDs, r.ProcessNodeID)
	}
	if err := s.countServed(rows, nodeIDs, runningParts(claims)); err != nil {
		log.Printf("station view: served pass: %v", err)
		return nil
	}
	s.markWaiting(rows, styleOf, nodeIDs)
	return rows
}

// stampStageTwoCard sets each stage-2 tile's bare-cart card (StageTwoSide,
// PartialParts, FullParts, from Loader.StageTwoParts): the live claims whose
// inbound source is one of the loader's windows or a group holding one, from
// the served pass's claims and the node list's group membership (GroupMembers,
// read once, and only on a board with a stage-2 tile), once per loader.
// Nothing on any other tile.
func (s *StationService) stampStageTwoCard(tiles []store.StationNodeView, served []processes.ServedClaim) {
	var groups map[string][]string
	var claims []processes.NodeClaim
	byLoader := map[domain.LoaderID][]domain.StageTwoPart{}
	for i := range tiles {
		t := &tiles[i]
		if t.Loader == nil || !t.Loader.TakesBare || s.loaders == nil {
			continue
		}
		l, err := s.loaders.LoaderForNode(domain.NodeID(t.Node.CoreNodeName))
		if err != nil || l == nil {
			continue
		}
		parts, done := byLoader[l.ID()]
		if !done {
			if groups == nil {
				groups = s.GroupMembers()
				claims = make([]processes.NodeClaim, len(served))
				for j := range served {
					claims[j] = served[j].Claim
				}
			}
			parts = l.StageTwoParts(claims, groups)
			byLoader[l.ID()] = parts
		}
		t.Loader.StageTwoSide = domain.StageTwoSide(parts)
		t.Loader.PartialParts = domain.StageTwoPartCodes(parts, "")
		t.Loader.FullParts = domain.StageTwoPartCodes(parts, protocol.ClaimRoleConsume)
	}
}

// rowsOf builds the rows, uncounted, in the claims' order: each claim's parts
// the board serves, at each of the process's nodes the claim names. styleOf
// holds each row's style name, for Core's sourcing verdict.
func (s *StationService) rowsOf(claims []processes.ServedClaim, nodes []processes.Node,
	parts map[string]bool, roles map[protocol.ClaimRole]bool,
) (rows []domain.ServedRow, styleOf []string) {
	nodesAt := map[nodeKey][]processes.Node{}
	for _, n := range nodes {
		k := nodeKey{n.ProcessID, n.CoreNodeName}
		nodesAt[k] = append(nodesAt[k], n)
	}
	rows = []domain.ServedRow{}
	for _, sc := range claims {
		c := sc.Claim
		if !roles[c.Role] {
			continue
		}
		for _, part := range c.AllowedPayloads() {
			if !parts[part] {
				continue
			}
			for _, n := range nodesAt[nodeKey{sc.ProcessID, c.CoreNodeName}] {
				rows = append(rows, domain.ServedRow{
					Cell: sc.ProcessName, Part: part, Role: string(c.Role),
					Carrier: s.binTypeForPayload(part), CapacityUOP: c.UOPCapacity,
					Incoming: sc.Incoming, Node: c.CoreNodeName, ProcessID: sc.ProcessID,
					ProcessNodeID: n.ID, ReorderUOP: c.DemandLevel(),
				})
				styleOf = append(styleOf, sc.StyleName)
			}
		}
	}
	return rows, styleOf
}

// runningParts is the running claim's part at each node, for a bin whose part
// was never established (servedHave).
func runningParts(claims []processes.ServedClaim) map[nodeKey]string {
	out := map[nodeKey]string{}
	for _, sc := range claims {
		if !sc.Incoming {
			out[nodeKey{sc.ProcessID, sc.Claim.CoreNodeName}] = sc.Claim.PayloadCode
		}
	}
	return out
}

// countServed sets each row's count from two reads: the served nodes'
// runtimes and their active piles.
func (s *StationService) countServed(rows []domain.ServedRow, nodeIDs []int64, running map[nodeKey]string) error {
	runtimes, err := processes.RuntimesForNodes(s.db.DB, nodeIDs)
	if err != nil {
		return err
	}
	piles, err := lineside.ListActiveForNodes(s.db.DB, nodeIDs)
	if err != nil {
		return err
	}
	for i := range rows {
		r := &rows[i]
		r.HaveUOP = servedHave(runtimes[r.ProcessNodeID], piles[r.ProcessNodeID], r.Part,
			running[nodeKey{r.ProcessID, r.Node}])
	}
	return nil
}

// nodeKey names a process node by its process and Core node.
type nodeKey struct {
	processID int64
	coreNode  string
}

// boardServes is the parts the board's loaders serve and the claim roles of
// the cells they serve: consume where a loader fills, produce where an
// unloader empties (a stage 2 is an unloader whatever its role,
// TileLoader.ActsAsUnloader). No loader tile, no roles.
func boardServes(tiles []store.StationNodeView) (map[string]bool, map[protocol.ClaimRole]bool) {
	parts := map[string]bool{}
	roles := map[protocol.ClaimRole]bool{}
	for _, t := range tiles {
		if t.Loader == nil {
			continue
		}
		if !t.Loader.ActsAsUnloader() {
			roles[protocol.ClaimRoleConsume] = true
		} else {
			roles[protocol.ClaimRoleProduce] = true
		}
		for _, p := range t.Loader.Parts {
			parts[p.Code] = true
		}
	}
	return parts, roles
}

// isLoaderClaim is the loader predicate, applied here and nowhere else in the
// pass, in Go: a stored manual_swap claim, or any claim at a node Core's loader
// set names (where the loader answers and a stored claim is not read). A loader
// is not a cell it serves. Pinned by TestServedRows_LoaderClaimsAreNotRows,
// which seeds a non-loader claim beside a loader one.
func (s *StationService) isLoaderClaim(c processes.NodeClaim) bool {
	return c.IsLoaderNode() || s.LoaderHolds(c.CoreNodeName)
}

// servedHave is a row's count of its part: the node's bin when the bin holds
// the part, plus the node's active piles of the part. Nil when the node has no
// runtime row, which is a cell the Edge cannot see.
//
// The bin's part is the runtime's established identity
// (LinesidePayloadCode); where none was established, the running claim's part
// at the node, which is the part the engine books the bin's count under
// (Engine.binAtNode). Pinned by TestLoaderLineside_CreditsByPayload (engine).
func servedHave(rt *processes.RuntimeState, piles []lineside.Bucket, part, runningPart string) *int {
	if rt == nil {
		return nil
	}
	binPart := string(rt.LinesidePayloadCode)
	if !rt.LinesidePayloadKnown {
		binPart = runningPart
	}
	have := 0
	if binPart == part {
		have = rt.RemainingUOPCached
	}
	for _, b := range piles {
		if b.PayloadCode == part {
			have += b.Qty
		}
	}
	return &have
}

// markWaiting sets Waiting on the rows whose cell waits on the part from
// outside.
//
//   - Every row waits when its cell has an order for the part queued with
//     Core's reason: the cell has asked and nothing is coming. Read on every
//     pass (TestServedRows_Waiting, a running row with no incoming style).
//   - An incoming consume row also waits when Core's sourcing verdict for the
//     row's style lists its part in Missing ("the payloads no available bin
//     could satisfy", protocol.SourcingState). Read only while a row is
//     incoming (TestServedRows_StatementBudget): a running style's verdict is
//     not read, so a steady poll pays no sixth statement.
//   - A produce row waits on the queued order only. Core nets every claim
//     against the free bins of its part, produce claims included, so on a
//     produce row Missing says no free FULL bin of the part exists, which is
//     not what a press making it waits on (an empty).
//
// A failed read marks nothing from it: the row reads as not waiting.
func (s *StationService) markWaiting(rows []domain.ServedRow, styleOf []string, nodeIDs []int64) {
	type orderKey struct {
		node int64
		part string
	}
	queued := map[orderKey]bool{}
	waits, err := orders.ListQueuedWithReasonAt(s.db.DB, nodeIDs)
	if err != nil {
		log.Printf("station view: served pass: queued orders: %v", err)
	}
	for _, o := range waits {
		if o.ProcessNodeID != nil {
			queued[orderKey{*o.ProcessNodeID, o.PayloadCode}] = true
		}
	}
	type verdictKey struct{ process, style, part string }
	missing := map[verdictKey]bool{}
	if slices.ContainsFunc(rows, func(r domain.ServedRow) bool { return r.Incoming }) {
		verdicts, err := s.db.ListSourcingState()
		if err != nil {
			log.Printf("station view: served pass: sourcing state: %v", err)
		}
		for _, v := range verdicts {
			for _, p := range v.Missing {
				missing[verdictKey{v.ProcessID, v.StyleID, p}] = true
			}
		}
	}
	for i := range rows {
		r := &rows[i]
		r.Waiting = queued[orderKey{r.ProcessNodeID, r.Part}] ||
			(r.Incoming && r.Role == string(protocol.ClaimRoleConsume) && missing[verdictKey{r.Cell, styleOf[i], r.Part}])
	}
}
