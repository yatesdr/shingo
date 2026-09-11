package dispatch

import (
	"fmt"

	"shingo/protocol"
	binsstore "shingocore/store/bins"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// binsAtStep is what this plan finds on a node when it reaches step `at`: the
// bins on the node now, less the ones this plan's earlier pickups there take.
//
// ── ONE QUESTION, THREE ASKERS ────────────────────────────────────────────────
//
// Three checks ask whether a node is clear, and each used to ask it of the world
// NOW — the wrong moment for every step after the plan's first. A choreography
// can place onto a node that holds a bin right now because its own earlier step
// carries that bin away, and it can re-collect at a node that holds a bin right
// now because that bin is the one its earlier step takes, not the one it set
// down. Keep-staged combined is both: it lifts the kept bin off inbound staging,
// stages its own carrier there, and collects that carrier later.
//
//   - the destination gate (reserveComplexDestination) asks it of a declared
//     exclusive dropoff, before anything is reserved;
//   - the relay rule (reserveComplexPlan) asks it of a re-collect;
//   - the slot claim (confirmComplexPlan) asks it of the staging slot it is about
//     to take, and ConfirmSlotClaim re-checks the answer inside the claim.
//
// Asked a different way by any one of them, the same plan is clear to one check
// and blocked at the next — which is how the keep-staged combined supply got past
// the gate and then held a partial set for ever at the reserve.
//
// ── WHICH BIN AN EARLIER PICKUP TAKES ─────────────────────────────────────────
//
// One: the bin this order already holds there, by reservation or claim, else —
// while it holds none there yet — one nobody holds. A bin another order holds is
// not this plan's to take whatever its steps say, so it stays on the node, and so
// does this plan's own bin once it has lost the hold; both are exactly what the
// slot claim must still refuse. A pickup that re-collects a carrier this plan set
// down at the node earlier takes that carrier, not a bin that was already there.
//
// taken is what the earlier pickups take; remaining is what is left. A dropoff at
// `at` finds the node clear exactly when remaining is empty.
func (a *Allocator) binsAtStep(order *orders.Order, steps []resolvedStep, at int, node string) (
	remaining []*binsstore.Bin, taken []int64, err error) {
	if node == "" {
		return nil, nil, nil
	}
	n, err := a.db.GetNodeByDotName(node)
	if err != nil || n == nil {
		return nil, nil, nil // an unresolvable node holds nothing anyone can count
	}
	onNode, err := a.db.ListBinsByNode(n.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("list bins at %s: %w", node, err)
	}
	if len(onNode) == 0 {
		return nil, nil, nil
	}
	rows, err := a.db.ListReservationsByOrder(order.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("list reservations for order %d: %w", order.ID, err)
	}
	ours := make(map[int64]bool, len(rows))
	for _, r := range rows {
		if r.Kind == reservations.KindBin {
			ours[r.BinID] = true
		}
	}
	for _, b := range onNode {
		if b.ClaimedBy != nil && *b.ClaimedBy == order.ID {
			ours[b.ID] = true
		}
	}

	if at > len(steps) {
		at = len(steps)
	}
	takenSet := make(map[int64]bool)
	carried := 0 // carriers this plan has set down at the node and not yet re-collected
	for _, s := range steps[:at] {
		if s.Node != node {
			continue
		}
		switch s.Action {
		case protocol.ActionDropoff:
			carried++
		case protocol.ActionPickup:
			if carried > 0 {
				carried--
				continue
			}
			if b := takenByPickup(onNode, takenSet, ours); b != nil {
				takenSet[b.ID] = true
			}
		}
	}
	for _, b := range onNode {
		if takenSet[b.ID] {
			taken = append(taken, b.ID)
		} else {
			remaining = append(remaining, b)
		}
	}
	return remaining, taken, nil
}

// takenByPickup is the bin one earlier pickup takes off the node: one this order
// holds, else one nobody holds. nil when every bin left there is somebody else's.
func takenByPickup(onNode []*binsstore.Bin, taken, ours map[int64]bool) *binsstore.Bin {
	for _, b := range onNode {
		if !taken[b.ID] && ours[b.ID] {
			return b
		}
	}
	for _, b := range onNode {
		if !taken[b.ID] && b.ClaimedBy == nil && !b.HasPendingReservation && !b.Locked {
			return b
		}
	}
	return nil
}
