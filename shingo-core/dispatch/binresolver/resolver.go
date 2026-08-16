package binresolver

import (
	"fmt"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// ResolveResult carries the resolved node and optionally a specific bin.
type ResolveResult struct {
	Node *nodes.Node
	Bin  *bins.Bin // set when resolver identified a specific bin
}

// ResolveMode says which direction material moves relative to the synthetic
// node being resolved, which is the only thing this package needs to know in
// order to pick a child.
//
// It used to be a protocol.OrderType, and reading the parameter that way was
// misleading in both directions. No caller has ever passed an order's actual
// type — all four pass a literal — and no order row has ever carried "store";
// the value existed to mean "I am putting a bin INTO this group", which is a
// direction, not a kind of order. Meanwhile a reader seeing an OrderType here
// would reasonably expect order.OrderType to be a legal argument, and it is
// not: a complex order resolves each STEP, and one order's steps go both ways.
type ResolveMode string

const (
	// ResolveModeRetrieve: take a bin OUT of this node. Picks the child holding
	// the best matching bin.
	ResolveModeRetrieve ResolveMode = "retrieve"
	// ResolveModeStore: put a bin INTO this node. Picks the child with room,
	// consolidating with like payload where it can.
	ResolveModeStore ResolveMode = "store"
)

// NodeResolver resolves a synthetic node to a physical child node.
//
// asker names the order the resolution is FOR. Only the NGRP path consults it,
// and only to answer the dig-lock question about candidate lanes, but it sits
// on the interface rather than on the group resolver because the caller with
// the order in hand is out here — every production call site has one. Pass
// reservations.Anyone where none exists.
type NodeResolver interface {
	Resolve(syntheticNode *nodes.Node, mode ResolveMode, payloadCode string, binTypeID *int64, asker reservations.DigAsker) (*ResolveResult, error)
}

// DefaultResolver resolves synthetic nodes using the database.
// For NGRP (node group) nodes, it delegates to the GroupResolver for two-level resolution.
//
// DB is declared as the narrow Store interface (satisfied by *store.DB)
// so algorithm unit tests can substitute a fake. See store.go for the
// method set.
type DefaultResolver struct {
	DB       Store
	DebugLog func(string, ...any)
}

// Compile-time assertion that *DefaultResolver satisfies NodeResolver.
var _ NodeResolver = (*DefaultResolver)(nil)

func (r *DefaultResolver) dbg(format string, args ...any) {
	if fn := r.DebugLog; fn != nil {
		fn(format, args...)
	}
}

// Resolve selects the best physical child of a synthetic node for the given
// direction of travel.
func (r *DefaultResolver) Resolve(syntheticNode *nodes.Node, mode ResolveMode, payloadCode string, binTypeID *int64, asker reservations.DigAsker) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(syntheticNode.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", syntheticNode.Name, err)
	}
	if len(children) == 0 {
		return nil, fmt.Errorf("synthetic node %s has no children", syntheticNode.Name)
	}

	// Delegate to group resolver for NGRP nodes
	if syntheticNode.NodeTypeCode == protocol.NodeClassNGRP {
		gr := &GroupResolver{DB: r.DB, DebugLog: r.DebugLog}
		switch mode {
		case ResolveModeRetrieve:
			return gr.ResolveRetrieve(syntheticNode, payloadCode, asker)
		case ResolveModeStore:
			return gr.ResolveStore(syntheticNode, payloadCode, binTypeID, asker)
		}
	}

	switch mode {
	case ResolveModeRetrieve:
		node, err := r.resolveRetrieve(children, payloadCode)
		if err != nil {
			return nil, err
		}
		return &ResolveResult{Node: node}, nil
	case ResolveModeStore:
		node, err := r.resolveStore(children, payloadCode)
		if err != nil {
			return nil, err
		}
		return &ResolveResult{Node: node}, nil
	default:
		// Unreachable by construction now that the parameter is a two-value
		// mode rather than the open OrderType set. Kept because deleting it
		// would be a behavior change on a path nothing exercises, and this
		// commit is a retype. An empty-carrier retrieve used to land here as
		// insurance; it now maps to ResolveModeRetrieve at the call site, which
		// is where the decision belongs.
		for _, c := range children {
			if c.Enabled {
				return &ResolveResult{Node: c}, nil
			}
		}
		return nil, fmt.Errorf("no enabled children for synthetic node %s", syntheticNode.Name)
	}
}

// resolveRetrieve returns the FIRST enabled child holding any bin that satisfies
// payloadCode, taking children in the order given.
//
// It does NOT rank by age, in either dimension: it makes no comparison across
// children, and within a child it accepts the first bin ListBinsByNode returns,
// which is ordered by bin id DESC. The previous comment here claimed it found
// "the child node with the oldest unclaimed bin", which was true in neither
// respect. Cross-child age ranking exists only in GroupResolver, via binTimestamp.
//
// UNREACHABLE FROM PRODUCTION, and the doc is kept rather than the code deleted
// so the next reader learns that here instead of re-deriving it. Resolve routes
// every NGRP node to GroupResolver before this switch, and both retrieve-mode
// entry points already gate on NGRP themselves (source_finder's tier 1 and
// complex_steps' group branch), so no caller can reach this arm with a
// retrieve. The only synthetic node outside NGRP is _TRANSIT, which is never a
// source or a delivery target. Store mode is a different matter -- resolveStore
// IS reachable, because lifecycle_service gates on IsSynthetic alone.
//
// Consequence worth stating: because this never runs, its ordering is not a live
// FIFO defect. If a caller is ever generalised past the NGRP gate, that changes,
// and the ranking question has to be answered before it does.
func (r *DefaultResolver) resolveRetrieve(children []*nodes.Node, payloadCode string) (*nodes.Node, error) {
	for _, child := range children {
		if !child.Enabled {
			continue
		}
		bins, err := r.DB.ListBinsByNode(child.ID)
		if err != nil {
			r.dbg("resolveRetrieve: ListBinsByNode node=%s: %v", child.Name, err)
			continue
		}
		for _, b := range bins {
			if !isBinAvailableForRetrieve(b, payloadCode) {
				continue
			}
			return child, nil
		}
	}
	return nil, fmt.Errorf("no child node has an available unclaimed bin")
}

// resolveStore finds the best child node for storage (consolidation-first, then emptiest).
func (r *DefaultResolver) resolveStore(children []*nodes.Node, payloadCode string) (*nodes.Node, error) {
	var candidates []storageCandidate
	for _, child := range children {
		if !child.Enabled || child.IsSynthetic {
			continue
		}
		count, err := r.DB.CountBinsByNode(child.ID)
		if err != nil {
			r.dbg("resolveStore: CountBinsByNode node=%s: %v", child.Name, err)
			continue
		}
		inflight, _ := r.DB.CountActiveOrdersByDeliveryNode(child.Name)
		if count+inflight >= 1 {
			continue
		}

		hasMatch := false
		if payloadCode != "" {
			bins, _ := r.DB.ListBinsByNode(child.ID)
			for _, b := range bins {
				if b.PayloadCode == payloadCode {
					hasMatch = true
					break
				}
			}
		}
		candidates = append(candidates, storageCandidate{node: child, count: count, hasMatch: hasMatch})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no child node available for storage")
	}

	return bestStorageCandidate(candidates), nil
}
