package service

import (
	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// station_send_back.go — SEND BACK's line on the board: its facts from the
// board's rows (the view build), and the line decided once the bins copy's
// rows are in hand (StampBins). Every read is one the board already made.

// sendBackOutsides is, per board tile whose active claim is a produce press of
// the fed shape (domain.FedShape over the running style's claims), its outside
// position's process node, from the held node rows (currentBinNodes, no read).
// A tile whose outside has no process node row is left out, its line absent;
// so is every tile on the one poll the held rows are not current.
func (s *StationService) sendBackOutsides(nodes []processes.Node, active map[string]processes.NodeClaim) map[int64]processes.Node {
	var held map[curtainKey]processes.Node
	asked := false
	out := map[int64]processes.Node{}
	for _, n := range nodes {
		c, ok := active[n.CoreNodeName]
		if !ok || c.Role != protocol.ClaimRoleProduce {
			continue
		}
		if _, claimed := active[c.PairedCoreNode]; !domain.FedShape(c.PairedCoreNode, claimed) {
			continue
		}
		if !asked {
			lists, _ := s.currentBinNodes()
			held, asked = lists.nodes, true
		}
		if on, ok := held[curtainKey{n.ProcessID, c.PairedCoreNode}]; ok {
			out[n.ID] = on
		}
	}
	return out
}

// sendBackFacts is a tile's line facts from the board's rows: nil on a tile
// that is no fed produce press (its line is absent, and SendBackLine would say
// so). The bins rows come in StampBins (stampSendBack).
func sendBackFacts(node processes.Node, process *processes.Process, claim *processes.NodeClaim,
	runtime *processes.RuntimeState, b *boardData) *domain.SendBackFacts {
	if claim == nil || claim.Role != protocol.ClaimRoleProduce {
		return nil
	}
	_, claimed := b.activeClaims[claim.PairedCoreNode]
	if !domain.FedShape(claim.PairedCoreNode, claimed) {
		return nil
	}
	f := &domain.SendBackFacts{Produce: true, Paired: claim.PairedCoreNode, Outbound: claim.OutboundDestination,
		ChangeoverArmed: process.TargetStyleID != nil}
	on, ok := b.outsides[node.ID]
	f.OutsideNode = ok
	var outsideRows []domain.Order
	if ok {
		outsideRows = b.boardOrders[on.ID]
	}
	var slots []*int64
	if runtime != nil {
		slots = []*int64{runtime.ActiveOrderID, runtime.StagedOrderID}
	}
	f.OrderFacts(b.boardOrders[node.ID], outsideRows, slots...)
	return f
}

// stampSendBack decides a tile's line once the bins copy's rows are in hand:
// the outside's cart and the core's. Absent leaves the tile's SendBack nil.
func stampSendBack(tile *domain.StationNodeView, rows map[string]BoardBin) {
	f := tile.SendBackFacts
	if f == nil {
		return
	}
	if row, ok := rows[f.Paired]; ok {
		r := row.NodeBinsRow
		f.Outside = &r
	}
	if row, ok := rows[tile.Node.CoreNodeName]; ok {
		r := row.NodeBinsRow
		f.Core = &r
	}
	if line := domain.SendBackLine(*f); line.State != domain.SendBackAbsent {
		tile.SendBack = &line
	}
}
