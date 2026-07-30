package engine

import (
	"encoding/json"
	"fmt"

	"shingo/protocol"
)

// legPlacesBinAt reports whether this leg LEAVES A BIN at node when it finishes:
//
//	the leg has a dropoff at node with no LATER pickup FROM node.
//
// That is the whole definition, and the "later" matters — a leg may set a bin
// down at a node and take it away again (or take one away and set a fresh one
// down), so only the last bin-moving action at node decides the answer.
//
// This is the supply/evac discriminator. "Where does the leg END?" is the wrong
// question and got press-index wrong: a 3-position R2 sets a bin on the press
// MID-sequence and then carries on to re-index the next position, so it ends at
// the index node while being the leg that supplies the press. Ask where the bin
// comes to rest, not where the robot does.
//
// Verified against BuildTwoRobotSwapSteps / BuildTwoRobotPressIndexSwapSteps
// (material_orders.go) — the builders are the source of truth for these shapes,
// and supply_leg_classifier_test.go drives its table straight off them:
//
//	leg                    | steps                                          | at press | role
//	-----------------------|------------------------------------------------|----------|-------
//	two_robot A            | …pickup(STAGE) dropoff(PRESS)                   | true     | supply
//	two_robot B            | wait(PRESS) pickup(PRESS) dropoff(OUT)          | false    | evac
//	press-index R1 (2&3)   | wait(PRESS) pickup(PRESS) dropoff(OUT)          |          |
//	                       |   pickup(IN) dropoff(B|C)                       | false    | evac
//	press-index R2, 2-pos  | wait(B) pickup(B) dropoff(PRESS)                | true     | supply
//	press-index R2, 3-pos  | wait(B) pickup(B) dropoff(PRESS)                |          |
//	                       |   pickup(C) dropoff(B)                          | true     | supply
//
// The 3-position R2 row is the one a "final dropoff" test gets wrong: its last
// dropoff is the index node B, but the bin it left on the press is still there.
func legPlacesBinAt(steps []protocol.ComplexOrderStep, node string) bool {
	if node == "" {
		return false
	}
	placed := false
	for _, s := range steps {
		if s.Node != node {
			continue
		}
		switch s.Action {
		case protocol.ActionDropoff:
			placed = true
		case protocol.ActionPickup:
			placed = false // taken back off; a later dropoff can set it down again
		}
	}
	return placed
}

// orderPlacesBinAtAny reports whether a live order will place a bin at any of
// the given nodes. It is the destination question both changeover gates ask, and
// it is TWO-ARMED because the store keeps the answer in a different column for
// each order shape (`sqlite_ddl.go:111`):
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

// legPlacesBinAtJSON decodes a stored steps_json and applies legPlacesBinAt.
// Errors are returned, never swallowed: a leg whose steps can't be read cannot
// be classified, and guessing "evac" is what wipes a supply bin's manifest.
func legPlacesBinAtJSON(stepsJSON, node string) (bool, error) {
	steps, err := decodeSteps(stepsJSON)
	if err != nil {
		return false, err
	}
	return legPlacesBinAt(steps, node), nil
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
