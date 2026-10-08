package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// bareLine is what a line with a bare position gets instead of a swap. Every
// swap opens by lifting the bin at the line, and a press's swap then indexes
// the paired position's bin onto the line, so with no bin at either the swap has
// nothing to do there and its lift holds at Core. A line with no bin gets one
// plain order bringing a bin from the claim's inbound source; a press gets one
// more for each paired position that is bare; and a press whose line holds a
// bin but whose paired position does not gets that position's bin alone, and no
// swap until it is there.
//
// THE SAME DECISION FOR BOTH ROLES. Material handling is circular: a consume
// line is refilled with a full of its part and a produce line with an empty, and
// which of the two the bin is comes from the claim, not from this decision. The
// planners read it and their applies create the orders their role carries.
//
// The keep-staged spot is not decided here. When a right spare stands on it
// (requestClaim), the request moves the line's delivery onto the spot.
type bareLine struct {
	source string
	// dest is the line's own position, or "" when the line holds a bin and only
	// paired positions are bare.
	dest   string
	primes []SimplePrime
}

// lineHeld says the line holds a bin and only paired positions are bare: the
// plan is the primes alone, and none at all is the hold (every bare position
// already has a bin on its way).
func (b *bareLine) lineHeld() bool { return b.dest == "" }

// planBareLine answers "this line has a bare position": nil when Core reports a
// bin at the line and at every paired position (or did not answer for one; a
// missing entry reads occupied), else the deliveries the bare positions need.
// inbound names the paired positions a bin is already on its way to; they are
// not primed again, and while one of them is still bare no swap is built. Pure.
//
// IT IS A PROPOSAL. The map answers a narrower question than this asks. It
// is true in the case the decision was written for: somebody pulled the bin off
// by hand, and the position is still bare when a robot gets there. It is false
// during a swap. Between the robot lifting the old bin and setting the new one
// down, the position holds no bin AND already has one on its way, and Core
// correctly answers empty. A delivery decided in that window goes to a position
// that is about to be occupied, and its robot can never put it down (sim
// 2026-08-31, ALN_004: four cells locked in one run). This function is pure and
// cannot tell the two apart; the witness is the cell's own in-flight orders. So
// the caller gates a non-nil answer with gateLineRows when the line is bare,
// and with guardPairedPrimes when it is held, before it creates anything, and a
// caller that skips that gate reintroduces the race.
//
// INBOUND GATES THE ORDER, NOT THE SWAP. A position that is still physically
// bare cannot be indexed from, whether or not the bin filling it is already on
// its way, so no swap is built for as long as the position reads bare, and only
// the duplicate order is skipped. Building the swap as soon as the bin was on
// its way would hand the second press of a double-tap exactly the swap whose
// index leg cannot source.
//
// The paired positions are a press's only. A name the occupancy read was not
// asked about reads occupied, but a stale paired node on another mode is not a
// position that cell has, so it is not looked at.
func planBareLine(node *processes.Node, claim *processes.NodeClaim, occupancy, inbound map[string]bool) (*bareLine, error) {
	lineBare := !isOccupied(occupancy, claim.CoreNodeName)
	var bare, needPrime []string
	if claim.SwapMode == protocol.SwapModeTwoRobotPressIndex {
		for _, pos := range claim.ExtensionPositions() {
			if isOccupied(occupancy, pos) {
				continue
			}
			bare = append(bare, pos)
			if !inbound[pos] {
				needPrime = append(needPrime, pos)
			}
		}
	}
	if !lineBare && len(bare) == 0 {
		return nil, nil
	}
	if (lineBare || len(needPrime) > 0) && claim.InboundSource == "" {
		return nil, fmt.Errorf("node %s has no inbound source configured", node.Name)
	}
	b := &bareLine{source: claim.InboundSource}
	if lineBare {
		b.dest = claim.CoreNodeName
	}
	for _, pos := range needPrime {
		b.primes = append(b.primes, SimplePrime{Source: claim.InboundSource, Dest: pos})
	}
	return b, nil
}
