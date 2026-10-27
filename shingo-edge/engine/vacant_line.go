package engine

import (
	"fmt"
	"log"
	"maps"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// landingSettle is how long this Edge's own record of a landing at a cell
// outranks Core's occupancy reading the cell empty: long enough for Core's bins
// feed to catch up with the landing it announced, short enough that a carrier
// moved off by hand in Core (which records no lift at the node it leaves, so the
// Edge sees no departure) stops holding the cell occupied soon after.
const landingSettle = 30 * time.Second

// heldByItsOwnLanding is W1's leftover (the double-supply race, round 4's §4):
// the head reads occupied when this Edge recorded a landing there within
// landingSettle and Core's occupancy reads it empty. The landing binds the
// runtime (handler_bin_landed.go, landCarrier) before Core's feed row for the
// cell moves, and a request in that second read the cell "empty (no bin)" and
// downgraded the swap to a plain delivery onto the carrier that had just landed:
// a move that can never drop, with no evac leg, that every later ask reads as
// already pointing there (the sim's WELD-1, Core order 271). The record is the
// runtime the caller already read: no statement. Reports whether it held the
// head, so the caller still asks what is on its way (gateLineRows): Core's
// re-arm of a swap's intermediate drop lands too (handler_uop_adjustment.go), and
// W1's own state must still be refused while its swap is in flight.
func heldByItsOwnLanding(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim,
	occupancy map[string]bool, now time.Time) (map[string]bool, bool) {
	if runtime == nil || runtime.ActiveBinID == nil || runtime.LinesideSource != string(domain.CarrierFromLanding) ||
		now.Sub(runtime.LinesideAt) > landingSettle || isOccupied(occupancy, claim.CoreNodeName) {
		return occupancy, false
	}
	held := make(map[string]bool, len(occupancy)+1)
	maps.Copy(held, occupancy)
	held[claim.CoreNodeName] = true
	log.Printf("[request-material] node %s: Core reads it empty, but bin %d landed here %s ago by this Edge's record — not downgrading",
		node.Name, *runtime.ActiveBinID, now.Sub(runtime.LinesideAt).Round(time.Second))
	return held, true
}

// vacantLine is what a line with a position that is not ready gets beside, or
// instead of, its swap. Every swap opens by lifting the bin at the line, so a
// line with no bin gets one plain order bringing a bin from the claim's inbound
// source instead of a swap. A press's dependent positions get what
// chainReadiness says each needs (chain): a clear of a wrong carrier, then a
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
// line alone and judges the dependent positions by chainReadiness, which also
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
// index from them (chainReadiness).
type Readiness struct {
	// Clear is each wrong carrier to move off its position, as the bins table
	// holds it, so the move can name the bin.
	Clear []protocol.NodeBinsRow
	// Prime is each position a carrier is to be brought to: a vacant one with
	// nothing coming, and every position in Clear, whose carrier leaves first.
	Prime []string
}

// Count is the orders the chain makes: one clear per wrong carrier, one prime
// per position. Both plans' OrderCount add it.
func (r Readiness) Count() int { return len(r.Clear) + len(r.Prime) }

// chainReadiness reads a claim's dependent positions (DependentPositions) and
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
func chainReadiness(claim *processes.NodeClaim, occupancy map[string]bool, deck Deck) Readiness {
	var r Readiness
	for _, pos := range claim.DependentPositions() {
		if !isOccupied(occupancy, pos) {
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
// press's index pickup will not take, so the swap would hold for it: Core's
// bind, read from the bins-feed row (dispatch findAvailableForNeed, ground rule
// 5). The clear moves it off, and the prime brings the carrier that is right.
// No bin type is checked.
//
//   - Any position: a bare marker (the row's Bare). It is no empty to fill
//     (EmptyBinsOnly) and no stock of a part.
//   - Produce: a carrier holding a part, except a partial of the claim's part
//     when the claim names a partial destination. The index is Empty, and Core
//     finishes a partial only where a partial home exists (the Empty branch's
//     partialToFinishAt); it takes no other stamped carrier. An empty, and tops
//     & bottoms (a tote cart at 0, no part), are what the index carries to the
//     press: not wrong.
//   - Consume: a carrier that is not stock of the claim's part: an empty of any
//     type, tops & bottoms, another part, or the part's carrier counted at
//     nothing. The index takes stock of the part alone (binresolver.StockOnly).
//     A partial of the part is kept: Core feeds consume partials and the press
//     runs the next FIFO bin, half full or full (owner, round 4 Q3).
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
// when Core reports a bin at the line and chainReadiness finds nothing to clear
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
	ready := chainReadiness(claim, occupancy, deck)
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
