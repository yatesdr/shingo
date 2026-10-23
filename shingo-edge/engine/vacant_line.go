package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// vacantLine is what a line with a vacant position gets instead of a swap.
// Every swap opens by lifting the bin at the line, and a press's swap then
// indexes the paired position's bin onto the line, so with no bin at either the
// swap has nothing to do there and its lift holds at Core. A line with no bin
// gets one plain order bringing a bin from the claim's inbound source; a press
// gets one more for each dependent position ChainReadiness primes; and a press
// whose line holds a bin but whose dependent positions are not ready gets their
// bins alone, and no swap until they are there.
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
	dest   string
	primes []SimplePrime
}

// lineHeld says the line holds a bin and only its dependent positions are not
// ready: the plan is the primes alone, and none at all is the hold (every
// vacant position already has a bin on its way).
func (b *vacantLine) lineHeld() bool { return b.dest == "" }

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
// when Core reports a bin at the line and ChainReadiness finds every dependent
// position ready, else the deliveries the line and those positions need.
// occupancy is the request's occupancy map (a missing entry reads occupied);
// deck is what it read of the dependent positions. Pure.
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
// the caller gates a non-nil answer with gateLineRows when the line is vacant,
// and with guardPairedPrimes when it is held, before it creates anything, and a
// caller that skips that gate reintroduces the race.
//
// A CARRIER COMING GATES THE ORDER, NOT THE SWAP. A position that is still
// physically vacant cannot be indexed from, whether or not the bin filling it is
// already on its way, so no swap is built for as long as the position reads
// vacant, and only the duplicate order is skipped. Building the swap as soon as
// the bin was on its way would hand the second press of a double-tap exactly the
// swap whose index leg cannot source.
func planVacantLine(node *processes.Node, claim *processes.NodeClaim, occupancy map[string]bool, deck Deck) (*vacantLine, error) {
	lineVacant := !isOccupied(occupancy, claim.CoreNodeName)
	ready := ChainReadiness(claim, occupancy, deck)
	if !lineVacant && len(ready.Vacant) == 0 && len(ready.Clear) == 0 {
		return nil, nil
	}
	if (lineVacant || len(ready.Prime) > 0) && claim.InboundSource == "" {
		return nil, fmt.Errorf("node %s has no inbound source configured", node.Name)
	}
	b := &vacantLine{source: claim.InboundSource}
	if lineVacant {
		b.dest = claim.CoreNodeName
	}
	for _, pos := range ready.Prime {
		b.primes = append(b.primes, SimplePrime{Source: claim.InboundSource, Dest: pos})
	}
	return b, nil
}
