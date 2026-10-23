package protocol

// PlacesBinAt reports whether this leg LEAVES A BIN at node when it finishes:
//
//	the leg has a dropoff at node with no LATER pickup FROM node.
//
// That is the whole definition, and the "later" matters — a leg may set a bin
// down at a node and take it away again (or take one away and set a fresh one
// down), so only the last bin-moving action at node decides the answer.
//
// This is the supply/evac discriminator, and "is a carrier coming to node" for
// both modules: the Edge reads it over the steps it authored, Core over the
// steps_json it stores. "Where does the leg END?" is the wrong question and got
// press-index wrong: a 3-position R2 sets a bin on the press MID-sequence and
// then carries on to re-index the next position, so it ends at the index node
// while being the leg that supplies the press. Ask where the bin comes to rest,
// not where the robot does.
//
// Verified against the Edge's BuildTwoRobotSwapSteps /
// BuildTwoRobotPressIndexSwapSteps (shingo-edge/engine/material_orders.go) —
// the builders are the source of truth for these shapes, and the Edge's
// engine/supply_leg_classifier_test.go drives its table straight off them:
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
func PlacesBinAt(steps []ComplexOrderStep, node string) bool {
	if node == "" {
		return false
	}
	placed := false
	for _, s := range steps {
		if s.Node != node {
			continue
		}
		switch s.Action {
		case ActionDropoff:
			placed = true
		case ActionPickup:
			placed = false // taken back off; a later dropoff can set it down again
		}
	}
	return placed
}

// OrderPlacesBinAt reports whether a live order leaves a bin at node: a complex
// order by PlacesBinAt over its steps, any other type by its delivery node.
//
// KEYED ON THE TYPE, NOT ON WHETHER STEPS ARE PRESENT. Core persists a
// transport plan on plain orders too (the dispatcher writes a retrieve's or a
// move's planned steps at dispatch), so "has steps" would read a plain order's
// route as its destination; a plain order's bin rests at its delivery node,
// which is authoritative for it. A complex order's delivery node is a display
// value (Core stores its last dropoff), the wrong answer for a 3-position R2.
//
// The caller decodes the steps, and a caller that cannot read or decode a row
// treats it as placing: the Edge's orderPlacesBinAtAny and placingAtSQL, and
// Core's "coming" read.
func OrderPlacesBinAt(orderType OrderType, steps []ComplexOrderStep, deliveryNode, node string) bool {
	if orderType == OrderTypeComplex {
		return PlacesBinAt(steps, node)
	}
	return node != "" && deliveryNode == node
}
