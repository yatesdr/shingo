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
	for _, styleID := range claimStyleOrder(process, p) {
		if styleID == nil {
			continue
		}
		if claim, err := db.GetStyleNodeClaimByNode(*styleID, node.CoreNodeName); err == nil && claim != nil {
			return claim
		}
	}
	return nil
}

// claimStyleOrder is the precedence itself, as a list of styles to try.
//
// SHARED BY BOTH RESOLVERS ON PURPOSE. ResolveNodeClaim reads a row per node
// and NodeClaimSet.Resolve reads from rows already in hand, and the one thing
// that must never differ between them is which style wins. Written twice, a
// reversed pair would change every mid-changeover decision and nothing at all
// outside a changeover — invisible to any test that seeds a single style.
func claimStyleOrder(process *processes.Process, p ClaimPrecedence) [2]*int64 {
	if p == TargetStyleFirst {
		return [2]*int64{process.TargetStyleID, process.ActiveStyleID}
	}
	return [2]*int64{process.ActiveStyleID, process.TargetStyleID}
}

// NodeClaimSet answers ResolveNodeClaim's question for a whole walk from rows
// read once, instead of a point query per node.
//
// THE THREE PER-NODE WALKERS ARE WHY IT EXISTS. The level sweep, the
// parked-ticks monitor and the counter tick each walk a node list and ask this
// question about every row; on a store pinned to one SQLite connection
// (store.Open sets SetMaxOpenConns(1)) that is one serialised statement per
// node per pass, on a Pi, with the operator board's next poll waiting behind
// it.
//
// IT HANDS BACK THE SAME POINTER FOR THE SAME CLAIM, AND THAT IS DELIBERATE.
// Two process_nodes can name one core node (a shared window is the ordinary
// case), and the level sweep's evaluators MUTATE the claim they are given —
// evaluateCellLevel writes below_reorder_since and then sets the field. Under
// the per-node reads the second node re-read the row and saw the first node's
// stamp, so it did not stamp again. Sharing the pointer reproduces that
// exactly; handing out copies would not, and the second node would re-stamp a
// falling edge that had already been recorded, moving the episode's start time.
type NodeClaimSet struct {
	byStyle map[int64]map[string]*processes.NodeClaim
}

// ClaimStyleIDs collects every style a claim lookup for these processes could
// consult — the active and target styles, nils dropped. The caller passes the
// result to NodeClaimsForStyles, which is the one query.
func ClaimStyleIDs(procs ...*processes.Process) []int64 {
	out := make([]int64, 0, 2*len(procs))
	for _, p := range procs {
		if p == nil {
			continue
		}
		for _, styleID := range claimStyleOrder(p, ActiveStyleFirst) {
			if styleID != nil {
				out = append(out, *styleID)
			}
		}
	}
	return out
}

// NodeClaimsForStyles reads every live claim on the given styles in one query.
func (db *DB) NodeClaimsForStyles(styleIDs []int64) (*NodeClaimSet, error) {
	byStyle, err := processes.ClaimsByNodeForStyles(db.DB, styleIDs)
	if err != nil {
		return nil, err
	}
	return &NodeClaimSet{byStyle: byStyle}, nil
}

// Resolve is ResolveNodeClaim against the rows this set already holds. A style
// the set was not built for resolves to nil, which is the same "no opinion" a
// failed lookup gives — so a caller that forgets a style gets no claim rather
// than the wrong one.
func (s *NodeClaimSet) Resolve(process *processes.Process, node *processes.Node, p ClaimPrecedence) *processes.NodeClaim {
	if s == nil || process == nil || node == nil {
		return nil
	}
	for _, styleID := range claimStyleOrder(process, p) {
		if styleID == nil {
			continue
		}
		if claim := s.byStyle[*styleID][node.CoreNodeName]; claim != nil {
			return claim
		}
	}
	return nil
}
