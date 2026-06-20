package dispatch

import (
	"shingo/protocol"
	binsstore "shingocore/store/bins"
)

// emptyBinsOnly returns the candidates that are empty carriers (no bound
// payload) — the set eligible for a produce node's empty pickup leg. A
// single-carrier loader has one bin type, so any empty is interchangeable;
// carrier-type matching for multi-carrier loaders is a known follow-up (the
// same limitation the edge RequestEmptyBin path already TODOs).
func emptyBinsOnly(candidates []*binsstore.Bin) []*binsstore.Bin {
	out := make([]*binsstore.Bin, 0, len(candidates))
	for _, b := range candidates {
		if b.PayloadCode == "" {
			out = append(out, b)
		}
	}
	return out
}

// resolvePerBinDestinations simulates the step sequence to determine where each
// claimed bin ends up after all pickups and dropoffs complete. The bin identity
// is tracked by location: a pickup at node X grabs whichever bin was last
// dropped there.
//
// Returns a map of binID → final destination node name.
//
// Edge cases handled:
//   - Empty robot dropoff (pre-positioning): carrying == 0, dropoff is a no-op
//   - Ghost pickup (no bin at node): carrying stays 0
//   - Bin re-pickup: a bin dropped at staging then picked up again gets a new dest
func resolvePerBinDestinations(steps []resolvedStep, claimedBins map[string]int64) map[int64]string {
	// Which bin the robot is currently carrying (0 = empty)
	var carrying int64

	// Which bin is sitting at which node after being dropped
	binAtNode := make(map[string]int64, len(claimedBins))
	for nodeName, binID := range claimedBins {
		binAtNode[nodeName] = binID
	}

	// Last known dropoff destination per bin
	dest := make(map[int64]string, len(claimedBins))

	for _, step := range steps {
		switch step.Action {
		case protocol.ActionPickup:
			if binID, ok := binAtNode[step.Node]; ok {
				carrying = binID
				delete(binAtNode, step.Node) // bin leaves this node
			}
			// If no bin at this node, robot picks up nothing (ghost/pre-position)

		case protocol.ActionDropoff:
			if carrying != 0 {
				dest[carrying] = step.Node      // update final dest
				binAtNode[step.Node] = carrying // bin is now at this node
				carrying = 0
			}
			// If robot is empty, this is a pre-position drive (no-op for bin tracking)

		case protocol.ActionWait:
			// No bin movement
		}
	}

	return dest
}
