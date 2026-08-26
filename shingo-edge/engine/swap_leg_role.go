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
//	FLIPPED R1 (2&3)       | wait(PRESS) pickup(PRESS) dropoff(OUT)          | false    | evac
//	FLIPPED R2, 2-pos      | wait(B) pickup(B) dropoff(PRESS)                |          |
//	                       |   pickup(IN) dropoff(B)                         | true     | supply
//	FLIPPED R2, 3-pos      | wait(B) pickup(B) dropoff(PRESS)                |          |
//	                       |   pickup(C) dropoff(B) pickup(IN) dropoff(C)    | true     | supply
//
// The FLIPPED rows make the point that the flip does not move the roles: it
// moves the supermarket trip from R1 to R2, and the press pickup and dropoff
// - which is what decides the role - stay where they were.
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

// classifySwapLegsBySteps re-derives which of a resolved pair is the SUPPLY
// (the leg that leaves a bin on the process node) and which is the EVAC, by
// reading the legs' steps rather than the runtime slot they happen to sit in.
//
// -- WHY THIS EXISTS -----------------------------------------------------
//
// store.ResolveSwapPair labels the pair POSITIONALLY: staged->evac,
// active->supply. That is a two_robot assumption -- two_robot creates the
// supply as leg A and the evac as leg B -- and it is INVERTED for press-index,
// where leg A (R1) clears the press and leg B (R2) puts the fresh carrier on.
// The IndexRobotSupplies flip does NOT change that: it moves the supermarket
// trip between the legs, not the press pickup and dropoff, so R1 is the evac
// and R2 the supply in both shapes.
//
// The label decides which leg gets the operator's release disposition, and the
// disposition is what sets remaining_uop -- i.e. WHICH BIN'S MANIFEST CORE
// CLEARS.
//
// WHAT THE INVERSION ACTUALLY COSTS, precisely, because it is not the obvious
// answer. The full disposition lands on the real SUPPLY leg, but the steps-based
// supply-bin guard in releaseOrderWithFullLineside suppresses the manifest sync
// there (the ALN_002 safety net, and it holds). So nothing is wiped wrongly.
// What is lost is the other half: the real EVAC leg gets the BARE disposition,
// so the bin actually leaving the press is released with remaining_uop=nil and
// Core never clears its manifest. A consume press-index bin goes back to the
// supermarket still carrying the parts it no longer holds.
//
// And on the produce side the label costs a trigger rather than a manifest:
// MaybeCreateUnloaderFullIn fires on (not supply) AND capture_lineside, and
// under the inversion NEITHER leg satisfies both -- the real evac has an empty
// Mode, the real supply is caught by isSupply. Press-index produce presses have
// therefore never fired the downstream unloader full-in that two_robot presses
// do. Correcting the labels starts firing it, which is the one live behaviour
// change here.
//
// Resolving from STEPS is not a fourth re-derivation of the leg's role; it is
// the SAME one the Edge classifier and Core's two dispatch predicates already
// use (legPlacesBinAt). What was missing was a caller here, because
// ResolveSwapPair works from runtime pointers and a node task and never loads
// the orders at all.
//
// -- WHEN IT CANNOT TELL -------------------------------------------------
//
// Exactly one leg of a well-formed pair places a bin at the process node. If
// both do, neither does, or the steps will not decode, this returns ok=false
// and the caller KEEPS THE POSITIONAL LABELS -- today's behaviour, no worse --
// and logs what it saw. Refusing instead would take the operator's release
// button away over a classification detail, on the one action that has no
// other route.
func (e *Engine) classifySwapLegsBySteps(processNode string, posEvacID, posSupplyID int64) (evacID, supplyID int64, ok bool) {
	if processNode == "" {
		return 0, 0, false
	}
	aJSON, aErr := e.db.GetOrderStepsJSON(posEvacID)
	bJSON, bErr := e.db.GetOrderStepsJSON(posSupplyID)
	if aErr != nil || bErr != nil {
		e.logFn("swap-leg classify node=%s: cannot read steps (evac-slot %d: %v, supply-slot %d: %v) - keeping positional labels",
			processNode, posEvacID, aErr, posSupplyID, bErr)
		return 0, 0, false
	}
	aPlaces, aDecodeErr := legPlacesBinAtJSON(aJSON, processNode)
	bPlaces, bDecodeErr := legPlacesBinAtJSON(bJSON, processNode)
	if aDecodeErr != nil || bDecodeErr != nil {
		e.logFn("swap-leg classify node=%s: cannot decode steps (%d: %v, %d: %v) - keeping positional labels",
			processNode, posEvacID, aDecodeErr, posSupplyID, bDecodeErr)
		return 0, 0, false
	}
	if aPlaces == bPlaces {
		// Both or neither leaves a bin on the press. Not a pair this function
		// can speak about - and worth saying out loud, because it means the
		// two legs are not the swap the caller thinks they are.
		e.logFn("swap-leg classify node=%s: BOTH legs place=%v (orders %d, %d) - not a supply/evac pair, keeping positional labels",
			processNode, aPlaces, posEvacID, posSupplyID)
		return 0, 0, false
	}
	if aPlaces {
		return posSupplyID, posEvacID, true // inverted: the "evac" slot holds the supply
	}
	return posEvacID, posSupplyID, true
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
