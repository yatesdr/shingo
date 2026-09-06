// claim_resolution.go — the one place that answers "which style's claim
// governs this node", and the two different questions people mean by it.

package store

import "shingoedge/store/processes"

// ClaimPrecedence names which style wins when a process is mid-changeover and
// both an active and a target style claim the node.
//
// THERE ARE TWO HONEST ANSWERS AND THE TREE HELD BOTH WITHOUT SAYING SO.
// Before this type there were two hand-rolled resolvers with opposite
// precedence and no shared name: engine's requestedClaimForProcess preferred the
// active style, and orders.Manager.resolveClaimForNode preferred the target
// whenever a target claim existed. Each was right for its own caller and each
// read like the general answer, so the disagreement was invisible — and the
// requested-side one leaked into paths describing a resident carrier, because
// the payload backfill runs for any order created with an empty payload code.
//
// Naming the choice does not make either resolver wrong. It makes picking one
// a decision a reader can see.
type ClaimPrecedence int

// BOTH CONSTANTS SELECT A STYLE. Neither can see a carrier: ResolveNodeClaim
// reads process.ActiveStyleID and process.TargetStyleID and nothing else — no
// active_bin_id, no lineside payload, nothing Core holds. They were named
// ResidentFirst and RequestedFirst, and ResidentFirst's doc said it answered
// "which claim governs what is STANDING on this node now", which it cannot
// know. That is the exact conflation this whole vocabulary exists to split,
// spelled in the names of the type that was supposed to end it. The names now
// say which style, and the question about what is standing there is answered
// by the runtime row's lineside payload instead.
const (
	// ActiveStyleFirst prefers the ACTIVE style's claim, falling back to the
	// target for a freshly-added node the active style does not claim yet.
	//
	// The right question for anything describing the cell AS IT IS CONFIGURED
	// NOW — a count's owner, a release, an evacuation's geometry. It is not
	// the answer to what carrier is on the node.
	ActiveStyleFirst ClaimPrecedence = iota

	// TargetStyleFirst prefers the TARGET style when it claims this node:
	// "which claim should something newly asked for be for". Mid-changeover a
	// new order is fetching the incoming style's material, so the target
	// claim is what should name its payload and its routing.
	//
	// The right question only when the subject of the sentence does not exist
	// yet. Asking it about a carrier already on the cell gets the incoming
	// style's answer about the outgoing style's bin.
	TargetStyleFirst
)

// ResolveNodeClaim returns the style_node_claims row governing this node under
// the given precedence, or nil for every reason a lookup can fail. Callers
// treat nil as "no opinion", never as a default.
func (db *DB) ResolveNodeClaim(process *processes.Process, node *processes.Node, p ClaimPrecedence) *processes.NodeClaim {
	if process == nil || node == nil {
		return nil
	}
	first, second := process.ActiveStyleID, process.TargetStyleID
	if p == TargetStyleFirst {
		first, second = process.TargetStyleID, process.ActiveStyleID
	}
	for _, styleID := range []*int64{first, second} {
		if styleID == nil {
			continue
		}
		if claim, err := db.GetStyleNodeClaimByNode(*styleID, node.CoreNodeName); err == nil && claim != nil {
			return claim
		}
	}
	return nil
}
