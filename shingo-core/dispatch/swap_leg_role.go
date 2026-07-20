package dispatch

import (
	"encoding/json"

	"shingo/protocol"
)

// The two step-shape predicates that decide a swap leg's ROLE. Both read the
// leg's own steps, which are the only thing that actually describes what it
// does. They exist because role used to be inferred from geometry —
// `DeliveryNode != ProcessNode` — and that inference is wrong:
//
//   - Core's order.DeliveryNode is not the Edge's. Edge never sends one
//     (ComplexOrderRequest has no such field); Core DERIVES it from the steps
//     via extractEndpoints, which takes the last pickup-OR-dropoff. For a
//     press-index R1 that is the index node, so R1 has always read as "delivers
//     away from the line" — i.e. as a removal leg needing a sibling's help —
//     even though it fetches its own replacement carrier.
//   - A 3-position press-index R2 drops a bin ON the line and then carries on to
//     re-index the next position, so it too ends away from the line while being
//     the leg that supplies it.
//
// Ask the steps what the leg does to the LINE BIN. Nothing else answers it.
// Mirrors legPlacesBinAt in shingo-edge/engine/swap_leg_role.go — same question,
// same answer, both sides of the wire.

// legTakesLineBin reports whether the leg lifts the line node's bin and does not
// put one back: a pickup at processNode with no dropoff at processNode. That is
// the evac shape.
//
// Verified against the Edge builders (material_orders.go):
//
//	two_robot A            dropoff(LINE), no pickup(LINE)         → false (supply)
//	two_robot B            pickup(LINE), no dropoff(LINE)         → TRUE  (evac)
//	press-index R1 (2&3)   pickup(LINE), dropoff(OUT/IN/B|C)      → TRUE  (evac)
//	press-index R2 (2&3)   pickup(B), dropoff(LINE)               → false (supply)
//	single_robot           pickup(LINE) AND dropoff(LINE)         → false (self-contained)
//	sequential removal     pickup(LINE), dropoff(OUT)             → TRUE  (evac, but sibling-less)
func legTakesLineBin(steps []resolvedStep, processNode string) bool {
	if processNode == "" {
		return false
	}
	tookBin := false
	for _, s := range steps {
		if s.Node != processNode {
			continue
		}
		switch s.Action {
		case protocol.ActionPickup:
			tookBin = true
		case protocol.ActionDropoff:
			return false // it puts a bin back on the line — not an evac
		}
	}
	return tookBin
}

// legPlacesLineBin reports whether the leg drives a bin ONTO the line node and
// does not also lift one off: a dropoff at processNode with no pickup at
// processNode. That is the supply/index shape — the leg that fills the shared
// line position, and so must not commit until whatever occupies that position
// has been cleared. The mirror of legTakesLineBin, same both sides of the wire.
//
// Verified against the Edge builders (material_orders.go):
//
//	two_robot A (supply)   dropoff(LINE), no pickup(LINE)         → TRUE  (filler)
//	press-index R2 (2&3)   pickup(B), dropoff(LINE)               → TRUE  (filler/index)
//	press-index R1 (2&3)   pickup(LINE), dropoff(OUT/IN/B|C)      → false (evac)
//	two_robot B (evac)     pickup(LINE), no dropoff(LINE)         → false (evac)
//	single_robot           pickup(LINE) AND dropoff(LINE)         → false (self-contained)
func legPlacesLineBin(steps []resolvedStep, processNode string) bool {
	if processNode == "" {
		return false
	}
	placedBin := false
	for _, s := range steps {
		if s.Node != processNode {
			continue
		}
		switch s.Action {
		case protocol.ActionDropoff:
			placedBin = true
		case protocol.ActionPickup:
			return false // it also lifts the line's bin — evac / self-contained, not a pure filler
		}
	}
	return placedBin
}

// legSecuresOwnReplacement reports whether the leg fetches a bin from somewhere
// other than the line — i.e. it brings a replacement INTO the swap itself and so
// does not depend on a sibling to secure one.
//
// A two_robot evac (wait → pickup(LINE) → dropoff(OUT)) has exactly one pickup:
// it only removes, and must wait for its supply sibling to claim before it pulls
// the line's bin, or the line strands (ALN_003). A press-index R1
// (pickup(LINE) → dropoff(OUT) → pickup(INBOUND) → dropoff(INDEX)) has a second
// pickup: it collects the fresh carrier itself. Holding it on a sibling is what
// deadlocked the swap — R1 waited on R2's claim while R2's only source was the
// index position R1 had not filled yet.
func legSecuresOwnReplacement(steps []resolvedStep) bool {
	pickups := 0
	for _, s := range steps {
		if s.Action == protocol.ActionPickup {
			pickups++
		}
	}
	return pickups > 1
}

// decodeSteps parses a stored steps_json. Returns nil (and false) when the steps
// cannot be read — callers must decide what "unknown shape" means for them
// rather than silently treating it as a particular role.
func decodeSteps(stepsJSON string) ([]resolvedStep, bool) {
	if stepsJSON == "" {
		return nil, false
	}
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		return nil, false
	}
	return steps, true
}
