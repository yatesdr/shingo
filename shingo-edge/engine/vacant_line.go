package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// vacantLine is what a line with a position that is not ready gets beside, or
// instead of, its swap. Every swap opens by lifting the bin at the line, so a
// line with no bin gets one plain order bringing a bin from the claim's inbound
// source instead of a swap. A press's dependent positions get what
// ChainReadiness says each needs (chain): a clear of a wrong carrier, then a
// prime; with the line holding a bin, those go AHEAD of the swap, which is
// queued at Core behind them (W5: the pair waits for the carrier coming to its
// on-deck, or fetches one to a vacant on-deck itself).
//
// THE SAME DECISION FOR BOTH ROLES. Material handling is circular: a consume
// line is refilled with a full of its part and a produce line with an empty, and
// which of the two the bin is comes from the claim, not from this decision. The
// planners read it and their applies create the orders their role carries.
//
// The keep-staged spot is not decided here. When a right spare stands on it
// (requestClaim), the request moves the line's delivery onto the spot.
type vacantLine struct {
	source string
	// dest is the line's own position, or "" when the line holds a bin and only
	// its dependent positions are not ready.
	dest  string
	chain Readiness
}

// vacantPositions is the one answer, from an occupancy map, to "which of this
// claim's positions hold no bin": the line's own, and its dependent positions.
// A name missing from occupancy reads occupied. A changeover splits a press per
// position on it (FanOutPressIndex, from the seats); REQUEST asks it for the
// line alone and judges the dependent positions by ChainReadiness, which also
// knows what stands on them.
func vacantPositions(claim *processes.NodeClaim, occupancy map[string]bool) (lineVacant bool, paired []string) {
	lineVacant = !isOccupied(occupancy, claim.CoreNodeName)
	for _, pos := range claim.DependentPositions() {
		if !isOccupied(occupancy, pos) {
			paired = append(paired, pos)
		}
	}
	return lineVacant, paired
}

// Deck is what a request read of its claim's dependent positions beyond their
// occupancy: the bins-table row standing at each (claimOccupancy; the carrier's
// identity) and whether a live order is placing a carrier there (PlacingAt, by
// the legs' places; never NodeBinsRow.Inbound, which reads delivery_node and
// misses complex legs).
type Deck struct {
	Rows   map[string]protocol.NodeBinsRow
	Coming map[string]bool
}

// Readiness is what a claim's dependent positions need before its swap can
// index from them (ChainReadiness).
type Readiness struct {
	// Clear is each wrong carrier to move off its position, as the bins table
	// holds it, so the move can name the bin.
	Clear []protocol.NodeBinsRow
	// Prime is each position a carrier is to be brought to: a vacant one with
	// nothing coming, and every position in Clear, whose carrier leaves first.
	Prime []string
	// Vacant is each position with nothing on it, a carrier coming or not.
	Vacant []string
}

// Count is the orders the chain makes: one clear per wrong carrier, one prime
// per position. Both plans' OrderCount add it.
func (r Readiness) Count() int { return len(r.Clear) + len(r.Prime) }

// ChainReadiness reads a claim's dependent positions (DependentPositions) and
// says what each needs before the swap can index from it, for both roles:
//
//   - vacant: a carrier, unless one is already coming (deck.Coming);
//   - a wrong carrier (wrongCarrier): cleared off, then a carrier brought,
//     unless a leg is placing there, which a clear would collide with;
//   - a right carrier, or one the table could not identify: nothing. Unknown
//     builds the swap, and Core queues it (owner, round 4): a Wait here would
//     stop the press on a Core blip.
//
// Pure. occupancy is the request's occupancy map (a name missing or not known
// to Core reads occupied), so a position is vacant here exactly when it is
// vacant to the line's half of the plan.
func ChainReadiness(claim *processes.NodeClaim, occupancy map[string]bool, deck Deck) Readiness {
	var r Readiness
	for _, pos := range claim.DependentPositions() {
		if !isOccupied(occupancy, pos) {
			r.Vacant = append(r.Vacant, pos)
			if !deck.Coming[pos] {
				r.Prime = append(r.Prime, pos)
			}
			continue
		}
		row, ok := deck.Rows[pos]
		if !ok || row.State != protocol.BinsStateOccupied || deck.Coming[pos] || !wrongCarrier(claim, row) {
			continue
		}
		r.Clear = append(r.Clear, row)
		r.Prime = append(r.Prime, pos)
	}
	return r
}

// wrongCarrier says the carrier standing on a dependent position is one the
// swap must not set on the press. Its identity is the bins-feed row's; no bin
// type is checked.
//
//   - Any position: a bare marker (the row's Bare). Core's on-deck pickup drops
//     a bare type (EmptyBinsOnly), so the pair would hold for good.
//   - Produce: a carrier holding a part, except a partial of the claim's part
//     when the claim names a partial destination. Core finishes a partial only
//     where a partial home exists (dispatch/allocator.go, the Empty branch's
//     partialToFinishAt), and its on-deck pickup takes no stamped carrier
//     otherwise. An empty, and tops & bottoms (a tote cart at 0, no part), are
//     what the index carries to the press: not wrong.
//   - Consume: a carrier that does not hold the claim's part: an empty of any
//     type, tops & bottoms, another part, or the part's carrier counted at
//     nothing. Core's consume pickup DOES take a part-less carrier (its payload
//     test passes a blank payload; dispatch
//     TestSpotPickup_HowTheStepFlagJudgesTheBin), so the clear is what keeps it
//     off the press. A partial of the part is kept: Core feeds consume partials
//     and the press runs the next FIFO bin, half full or full (owner, round 4
//     Q3).
func wrongCarrier(claim *processes.NodeClaim, row protocol.NodeBinsRow) bool {
	switch {
	case row.Bare:
		return true
	case claim.Role == protocol.ClaimRoleConsume:
		return row.PayloadCode != claim.PayloadCode || !row.Stocked
	case row.PayloadCode == "":
		return false
	default:
		partialKept := row.PayloadCode == claim.PayloadCode && row.Partial && claim.PartialDestination != ""
		return !partialKept
	}
}

// planVacantLine answers "this line has a position that is not ready": nil
// when Core reports a bin at the line and ChainReadiness finds nothing to clear
// or prime, else the line's delivery (when it is vacant) and the chain. occupancy
// is the request's occupancy map (a missing entry reads occupied); deck is what
// it read of the dependent positions. Pure.
//
// IT IS A PROPOSAL. The map answers a narrower question than this asks. It
// is true in the case the decision was written for: somebody pulled the bin off
// by hand, and the position is still vacant when a robot gets there. It is false
// during a swap. Between the robot lifting the old bin and setting the new one
// down, the position holds no bin AND already has one on its way, and Core
// correctly answers empty. A delivery decided in that window goes to a position
// that is about to be occupied, and its robot can never put it down (sim
// 2026-08-31, ALN_004: four cells locked in one run). This function is pure and
// cannot tell the two apart; the witness is the cell's own in-flight orders. So
// the caller gates a vacant line's delivery with gateLineRows before it creates
// anything, and a dependent position is primed only with nothing placing there
// (deck.Coming), the swap that would fill it having been refused already
// (guardLineRequest).
//
// A CARRIER COMING SKIPS THE ORDER, AND THE SWAP IS QUEUED. A dependent position
// that is vacant, or whose wrong carrier is being cleared, cannot be indexed from
// yet, but the swap is built anyway and goes behind the chain: Core holds the
// pair for the carrier coming there (waiting_for_outside) and fetches one to a
// vacant on-deck with nothing coming, so the press indexes the moment it can.
// Only the duplicate order is skipped; a second tap meets the swap in flight.
func planVacantLine(node *processes.Node, claim *processes.NodeClaim, occupancy map[string]bool, deck Deck) (*vacantLine, error) {
	lineVacant := !isOccupied(occupancy, claim.CoreNodeName)
	ready := ChainReadiness(claim, occupancy, deck)
	if !lineVacant && ready.Count() == 0 {
		return nil, nil
	}
	if (lineVacant || len(ready.Prime) > 0) && claim.InboundSource == "" {
		return nil, fmt.Errorf("node %s has no inbound source configured", node.Name)
	}
	if len(ready.Clear) > 0 && claim.OutboundDestination == "" {
		return nil, fmt.Errorf("node %s has no outbound destination configured", node.Name)
	}
	b := &vacantLine{source: claim.InboundSource, chain: ready}
	if lineVacant {
		b.dest = claim.CoreNodeName
	}
	return b, nil
}
