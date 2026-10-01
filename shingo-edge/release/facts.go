package release

import (
	"encoding/json"
	"fmt"
	"slices"

	"shingo/protocol"
)

// Facts are a leg's static release facts: what its steps say about the bins a
// release lets it move. The Edge authors the steps, so it computes these once,
// when it creates the order, and persists them on the row (orders.release_facts).
// Plan and the board then read them instead of decoding steps_json.
//
// Every question is answered for any node, not only the order's own process
// node, so a door that asks about another position of the cell (the collision
// guard's evac arm) gets the same answer the steps would give.
type Facts struct {
	// Places is every node the leg leaves a bin at: a dropoff there with no
	// later pickup from it (PlacesBinAt).
	Places []string `json:"places,omitempty"`
	// Departs maps each node the leg lifts a bin at, in the segment between its
	// first station wait and its next wait of any kind, to that pickup's step
	// index. It is the bin a release takes away.
	Departs map[string]int `json:"departs,omitempty"`
	// Touches is every node the leg picks up at or drops at after its first
	// station wait, in step order: the nodes a release lets a bin cross, which
	// the light curtain gates.
	Touches []string `json:"touches,omitempty"`
	// Role and DepartingStep say the same for the order's process node, as the
	// order was created: "departing" (its release lifts the node's bin),
	// "placing" (it leaves a bin on the node) or "neither". A leg that does both
	// is departing. DepartingStep is the step index of that lift, nil when it
	// has none.
	Role          string `json:"role"`
	DepartingStep *int   `json:"departing_step,omitempty"`
	// Purposes is each station wait's purpose, by its ordinal among the
	// leg's station waits (the number Core's OrderStaged reports). An
	// untagged wait's entry is "".
	Purposes []string `json:"purposes,omitempty"`
}

// The roles a leg can have at its process node.
const (
	RoleDeparting = "departing"
	RolePlacing   = "placing"
	RoleNeither   = "neither"
)

// FactsFromSteps computes a leg's facts from its steps. processNode is the
// order's process node (the line node), for Role and DepartingStep.
func FactsFromSteps(steps []protocol.ComplexOrderStep, processNode string) Facts {
	var f Facts
	// Places: every node the leg sets a bin down on and does not take it
	// back from (PlacesBinAt), in the order the leg first visits it.
	seen := map[string]bool{}
	for _, s := range steps {
		if s.Action == protocol.ActionDropoff && s.Node != "" && !seen[s.Node] {
			seen[s.Node] = true
			if PlacesBinAt(steps, s.Node) {
				f.Places = append(f.Places, s.Node)
			}
		}
	}
	// Purposes: one per station wait, in order.
	for _, s := range steps {
		if s.Action == protocol.ActionWait && protocol.IsStationWaitKind(s.WaitKind) {
			f.Purposes = append(f.Purposes, s.Purpose)
		}
	}
	// Departs and Touches: everything after the first station wait.
	start := -1
	for i, s := range steps {
		if s.Action == protocol.ActionWait && protocol.IsStationWaitKind(s.WaitKind) {
			start = i + 1
			break
		}
	}
	if start >= 0 {
		inSegment := true
		for i := start; i < len(steps); i++ {
			s := steps[i]
			if s.Action == protocol.ActionWait {
				inSegment = false
				continue
			}
			if (s.Action == protocol.ActionPickup || s.Action == protocol.ActionDropoff) && s.Node != "" {
				f.Touches = append(f.Touches, s.Node)
			}
			if inSegment && s.Action == protocol.ActionPickup && s.Node != "" {
				if _, seen := f.Departs[s.Node]; !seen {
					if f.Departs == nil {
						f.Departs = map[string]int{}
					}
					f.Departs[s.Node] = i
				}
			}
		}
	}
	switch {
	case f.DepartsFrom(processNode):
		step := f.Departs[processNode]
		f.Role, f.DepartingStep = RoleDeparting, &step
	case f.PlacesBinAt(processNode):
		f.Role = RolePlacing
	default:
		f.Role = RoleNeither
	}
	return f
}

// PlacesBinAt is the package's PlacesBinAt, answered from the stored facts.
func (f Facts) PlacesBinAt(node string) bool {
	return node != "" && slices.Contains(f.Places, node)
}

// DepartsFrom reports whether a release of the leg lifts a bin at node: the
// segment after its first station wait picks one up there. A leg with no
// station wait has no release, and departs from nowhere.
func (f Facts) DepartsFrom(node string) bool {
	_, ok := f.Departs[node]
	return node != "" && ok
}

// PurposeAt is the purpose of the leg's station wait with this ordinal, ""
// past its last one.
func (f Facts) PurposeAt(ordinal int) Purpose {
	if ordinal < 0 || ordinal >= len(f.Purposes) {
		return ""
	}
	return Purpose(f.Purposes[ordinal])
}

// PlacesAtAny returns the first of positions the leg leaves a bin at, or "".
func (f Facts) PlacesAtAny(positions []string) string {
	for _, p := range positions {
		if f.PlacesBinAt(p) {
			return p
		}
	}
	return ""
}

// Encode renders the facts for the orders.release_facts column.
func (f Facts) Encode() (string, error) {
	raw, err := json.Marshal(f)
	if err != nil {
		return "", fmt.Errorf("encode release facts: %w", err)
	}
	return string(raw), nil
}

// DecodeFacts reads an orders.release_facts value.
func DecodeFacts(raw string) (Facts, error) {
	var f Facts
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return Facts{}, fmt.Errorf("decode release facts: %w", err)
	}
	return f, nil
}

// PlacesBinAt reports whether this leg LEAVES A BIN at node when it finishes:
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
// and engine/supply_leg_classifier_test.go drives its table straight off them:
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
func PlacesBinAt(steps []protocol.ComplexOrderStep, node string) bool {
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
