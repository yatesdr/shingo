package engine

import (
	"encoding/json"
	"fmt"

	"shingo/protocol"
	"shingoedge/release"
	"shingoedge/store/orders"
)

// legPlacesBinAt is the supply/evac discriminator: whether this leg LEAVES A
// BIN at node when it finishes (release.PlacesBinAt, its one definition).
func legPlacesBinAt(steps []protocol.ComplexOrderStep, node string) bool {
	return release.PlacesBinAt(steps, node)
}

// orderPlacesBinAtAny reports whether a live order will place a bin at any of
// the given nodes. It is the destination question both changeover gates ask, and
// it is TWO-ARMED because the store keeps the answer in a different column for
// each order shape (the delivery_node comment on the orders table,
// store/schema/sqlite_ddl.go):
//
//	steps_json == ""  simple order — delivery_node is "authoritative for SIMPLE
//	                  orders (one bin, one destination)"
//	steps_json != ""  complex order — delivery_node is "effectively a DISPLAY
//	                  value; nothing correctness-critical reads it any more",
//	                  so the steps are the only truth
//
// WHY THE EMPTY CASE IS NOT A BLOCK, which is the trap this function exists to
// avoid. orderGatesCutover fails closed on empty steps, and `steps_json TEXT NOT
// NULL DEFAULT ”` is the DDL default — so a simple order there is not
// destination-tested at all, it is an unconditional yes. Reading steps per order
// is fine; inheriting that default is not. Empty steps means "simple order, use
// the delivery-node arm", never "gate".
//
// Fail-closed remains correct for the two cases where the shape cannot be
// established: an unreadable row and undecodable steps. Those are "we cannot
// prove this leg is irrelevant", which is different from "this leg has no steps
// because simple orders never write any".
func (e *Engine) orderPlacesBinAtAny(orderID int64, deliveryNode string, nodes []string) bool {
	stepsJSON, err := e.db.GetOrderStepsJSON(orderID)
	if err != nil {
		return true // cannot read the row — cannot prove it is irrelevant
	}
	if stepsJSON == "" {
		for _, n := range nodes {
			if n != "" && n == deliveryNode {
				return true
			}
		}
		return false
	}
	steps, err := decodeSteps(stepsJSON)
	if err != nil {
		return true // undecodable steps fail closed, as they do at the cutover gate
	}
	for _, n := range nodes {
		if legPlacesBinAt(steps, n) {
			return true
		}
	}
	return false
}

// orderRefillsNodeItself reports whether this order's OWN plan already brings
// coreNode its replacement carrier — the question handleSequentialBackfill has
// to ask before minting a second one.
//
// It is a property of the order, not of the order's origin. The two sequential
// Order A shapes come out of two builders and differ in exactly this:
//
//	BuildSequentialRemovalSteps        wait(N) pickup(N) dropoff(OUT)
//	  removes only — the position gets nothing back, so a backfill is the
//	  only thing that will feed it. false.
//	buildSequentialPerPositionSwap     wait(N) pickup(N) dropoff(OUT)
//	                                     pickup(IN) dropoff(N)
//	  a round trip — it fetches the new style's carrier and sets it down on
//	  the same position. true, and a backfill would be a SECOND carrier for
//	  one slot. (buildSequentialPerPositionEvacuate has the same tail and is
//	  covered by the same read.)
//
// Asking the shape rather than "is a changeover running" is what keeps the two
// answers from drifting: a builder that starts or stops refilling its own node
// changes this predicate's answer on the same commit that changes the plan.
//
// Pinned by TestSequentialChangeover_OnePositionGetsOneCarrier and its mirror
// TestSequentialSteadyState_RemovalStillMintsTheBackfill.
//
// DeliveryNode is passed for the SIMPLE-order arm only, and a sequential Order A
// is always complex, so in practice the steps answer. See orderPlacesBinAtAny
// for why the empty-steps case is the delivery-node arm and never a block.
func (e *Engine) orderRefillsNodeItself(order *orders.Order, coreNode string) bool {
	if order == nil || coreNode == "" {
		return false
	}
	return e.orderPlacesBinAtAny(order.ID, order.DeliveryNode, []string{coreNode})
}

func decodeSteps(stepsJSON string) ([]protocol.ComplexOrderStep, error) {
	if stepsJSON == "" {
		return nil, fmt.Errorf("no steps stored")
	}
	var steps []protocol.ComplexOrderStep
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		return nil, fmt.Errorf("decode steps: %w", err)
	}
	return steps, nil
}
