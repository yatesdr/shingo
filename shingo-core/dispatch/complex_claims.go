package dispatch

import (
	"encoding/json"
	"fmt"
	"log"

	"shingo/protocol"
	binsstore "shingocore/store/bins"
	"shingocore/store/orders"
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

// PlannedBinDestinations is the ONE derivation of "where is each of this order's
// bins due to end up", exported for callers outside this package.
//
// It exists so the settle-time drift tripwire in engine/ checks the SAME answer
// the allocator writes and the re-bind updates, rather than forming a second
// opinion. A tripwire that derives a fact independently is not measuring
// disagreement between a record and the plan; it is measuring disagreement
// between two derivations, and law 15 wants exactly one.
//
// claimedByNode maps a pickup node's dot-path to the bin claimed there — the same
// shape the allocator passes in, and the shape order_bins rows already carry as
// (node_name, bin_id).
func PlannedBinDestinations(stepsJSON string, claimedByNode map[string]int64) (map[int64]string, error) {
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		return nil, fmt.Errorf("parse plan for per-bin destinations: %w", err)
	}
	return resolvePerBinDestinations(steps, claimedByNode), nil
}

// refreshOrderBinDestinations re-derives every claimed bin's destination from the
// order's CURRENT plan and writes back any junction row that disagrees.
//
// ── WHY THIS EXISTS ───────────────────────────────────────────────────────
// order_bins.dest_node was written once, by the allocator, and updated by
// nothing. The gate re-bind moves a swap's lane leg to a different slot at
// release — so the robot drove to the new slot while the junction row still named
// the old one, and the whole-order settle then placed the bin at the row's stale
// value. One fact, two copies, one of them frozen (PLAN §R.25, D2).
//
// ── WHY IT RE-DERIVES INSTEAD OF PATCHING THE ONE ROW ─────────────────────
// The caller knows a STEP INDEX; the junction is keyed by BIN, and its step_index
// column names the PICKUP the allocator claimed at, not the dropoff being
// re-pointed. Mapping one to the other means walking the plan and tracking what
// the robot is carrying — which is precisely what resolvePerBinDestinations
// already does. Writing a second walk to answer the same question would be a
// second derivation of one fact, which is the disease this batch is treating.
//
// So the map is rebuilt from the junction rows themselves — node_name → bin_id is
// exactly the claimedBins map the allocator passed in — and fed back through the
// one derivation against the patched steps. Same input shape, same function, one
// answer.
//
// Best-effort and quiet on the ordinary path: no junction rows means a single-bin
// order, which has none by design. A write failure is logged, not returned — the
// plan and delivery_node are already correct and the append must not be blocked
// by a bookkeeping row; the disagreement counter at the settle sites is what makes
// a persistent failure visible rather than assumed away.
func (d *Dispatcher) refreshOrderBinDestinations(order *orders.Order) {
	rows, err := d.db.ListOrderBins(order.ID)
	if err != nil {
		log.Printf("dispatch: order %d re-bind could not read its junction rows (%v) — per-bin "+
			"destinations may now disagree with the plan", order.ID, err)
		return
	}
	if len(rows) == 0 {
		return // single-bin order: no junction, nothing to keep in step
	}
	claimed := make(map[string]int64, len(rows))
	for _, ob := range rows {
		claimed[ob.NodeName] = ob.BinID
	}
	dest, err := PlannedBinDestinations(order.StepsJSON, claimed)
	if err != nil {
		log.Printf("dispatch: order %d re-bind could not parse its own plan (%v) — per-bin "+
			"destinations left as they were", order.ID, err)
		return
	}
	for _, ob := range rows {
		want, ok := dest[ob.BinID]
		if !ok || want == "" || want == ob.DestNode {
			continue
		}
		n, uErr := d.db.UpdateOrderBinDestNode(order.ID, ob.BinID, want)
		if uErr != nil {
			log.Printf("dispatch: order %d bin %d destination %s → %s not recorded: %v",
				order.ID, ob.BinID, ob.DestNode, want, uErr)
			continue
		}
		if n == 0 {
			log.Printf("dispatch: order %d bin %d destination %s → %s matched no junction row — "+
				"the row was read a moment ago, so something deleted it mid-release",
				order.ID, ob.BinID, ob.DestNode, want)
			continue
		}
		d.dbg("lane gate: order %d bin %d destination re-recorded %s → %s (follows the re-bind)",
			order.ID, ob.BinID, ob.DestNode, want)
	}
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
