package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// bareLine is what a line with no bin on it gets instead of a swap. Every swap
// opens by lifting the bin at the line, so with none there the swap has nothing
// to do and its lift would hold at Core. One plain order brings a bin from the
// claim's inbound source to the line instead, and on a press one more brings a
// bin to each paired position that is bare too, so the next swap has something
// to index from.
//
// THE SAME DECISION FOR BOTH ROLES. Material handling is circular: a consume
// line is refilled with a full of its part and a produce line with an empty, and
// which of the two the bin is comes from the claim, not from this decision. The
// planners read it and their applies create the orders their role carries.
//
// The keep-staged spot is not decided here. When the right spare stands on it,
// planSpotForConsume and planSpotForProduce move the line's delivery onto the
// spot, because what stands there and what is leaving it are read after the
// plan, under the cell's lock.
type bareLine struct {
	source string
	dest   string
	primes []SimplePrime
}

// planBareLine answers "this line has no bin": nil when Core reports a bin at
// the line (or did not answer for it; a missing entry reads occupied), else the
// deliveries the bare line needs. Pure.
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
// the caller gates a non-nil answer with guardPositionSpokenFor or
// positionWorkedBy before it creates anything, and a caller that skips that gate
// reintroduces the race.
//
// The paired positions are a press's only. A name the occupancy read was not
// asked about reads occupied, but a stale paired node on another mode is not a
// position that cell has, so it is not looked at.
func planBareLine(node *processes.Node, claim *processes.NodeClaim, occupancy map[string]bool) (*bareLine, error) {
	if isOccupied(occupancy, claim.CoreNodeName) {
		return nil, nil
	}
	if claim.InboundSource == "" {
		return nil, fmt.Errorf("node %s has no inbound source configured", node.Name)
	}
	b := &bareLine{source: claim.InboundSource, dest: claim.CoreNodeName}
	if claim.SwapMode == protocol.SwapModeTwoRobotPressIndex {
		for _, pos := range claim.ExtensionPositions() {
			if !isOccupied(occupancy, pos) {
				b.primes = append(b.primes, SimplePrime{Source: claim.InboundSource, Dest: pos})
			}
		}
	}
	return b, nil
}
