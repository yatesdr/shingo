package engine

import (
	"log"

	"shingo/protocol"
)

// HandleBinEpochRefresh adopts a new generation stamp for the carrier already
// bound at a node. It writes nothing else.
//
// Core sends this when it discards a count for carrying a generation that has
// ended. That discard is the one moment in the system that holds all four
// facts at once — which carrier, which generation is current, which station is
// behind, and proof that it is behind — so it is where the repair belongs. The
// Edge adopts the stamp and its next count lands.
//
// The count is deliberately absent, and the handler must keep it that way. The
// Edge is the authority on how many parts are in the carrier; Core is holding a
// copy that is behind by exactly the counts it discarded. Writing Core's number
// back down here would overwrite the good number with the bad one.
//
// The shape is the one at operator_changeover_cancel.go's reconcile — ask what
// Core says about this node and adopt it — with the trigger widened from
// "somebody cancelled a changeover" to "the evidence says this node is behind".
func (e *Engine) HandleBinEpochRefresh(r protocol.BinEpochRefresh) {
	node, err := e.db.GetProcessNodeByCoreNodeName(r.CoreNodeName)
	if err != nil || node == nil {
		log.Printf("bin_epoch_refresh: process node %q not found: %v", r.CoreNodeName, err)
		return
	}
	rt, err := e.db.GetProcessNodeRuntime(node.ID)
	if err != nil || rt == nil {
		log.Printf("bin_epoch_refresh: runtime for node %s (id=%d) not found: %v", r.CoreNodeName, node.ID, err)
		return
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != r.BinID {
		// The refresh is about a carrier that is not here. Never rebind a slot
		// off this message: it carries no count, so binding would leave the new
		// carrier showing the departed one's number.
		log.Printf("bin_epoch_refresh: bin %d not active at node %s (active_bin_id=%v) — ignoring",
			r.BinID, r.CoreNodeName, rt.ActiveBinID)
		return
	}
	// Bin pointer passed back unchanged — the write names the carrier so the
	// store's monotonicity rule has something to compare against, and so a
	// refresh that lost a race to a newer stamp leaves the newer one alone.
	if err := e.db.SetProcessNodeActiveBinIDAndEpoch(node.ID, rt.ActiveBinID, r.Epoch); err != nil {
		log.Printf("bin_epoch_refresh: write epoch=%d for bin %d at node %s: %v",
			r.Epoch, r.BinID, r.CoreNodeName, err)
		return
	}
	log.Printf("bin_epoch_refresh: bin %d at node %s adopted epoch %d (was %d)",
		r.BinID, r.CoreNodeName, r.Epoch, rt.ActiveBinEpoch)
}
