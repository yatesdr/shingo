package dispatch

import (
	"shingo/protocol"
	binsstore "shingocore/store/bins"
)

// snapshotPickupBins lists the candidate bins at each distinct pickup node,
// keyed by dot-name, from the pre-claim state — the input BuildComplexPlan
// resolves the order's steps against. Best-effort and read-only: a node that
// fails to resolve or list is simply omitted, which BuildComplexPlan renders as
// the same "no bins at node" skip the claim path records on the identical failure.
func (d *Dispatcher) snapshotPickupBins(steps []resolvedStep) map[string][]*binsstore.Bin {
	out := make(map[string][]*binsstore.Bin)
	for _, s := range steps {
		if s.Action != protocol.ActionPickup {
			continue
		}
		if _, done := out[s.Node]; done {
			continue
		}
		node, err := d.db.GetNodeByDotName(s.Node)
		if err != nil {
			continue
		}
		listed, err := d.db.ListBinsByNode(node.ID)
		if err != nil {
			continue
		}
		out[s.Node] = listed
	}
	return out
}
