package binresolver

import (
	"fmt"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// ResolveResult carries the resolved node and optionally a specific bin.
type ResolveResult struct {
	Node *nodes.Node
	Bin  *bins.Bin // set when resolver identified a specific bin
}

// NodeResolver resolves a synthetic node to a physical child node.
type NodeResolver interface {
	Resolve(syntheticNode *nodes.Node, orderType protocol.OrderType, payloadCode string, binTypeID *int64) (*ResolveResult, error)
}

// DefaultResolver resolves synthetic nodes using the database.
// For NGRP (node group) nodes, it delegates to the GroupResolver for two-level resolution.
//
// DB is declared as the narrow Store interface (satisfied by *store.DB)
// so algorithm unit tests can substitute a fake. See store.go for the
// method set.
type DefaultResolver struct {
	DB       Store
	LaneLock *LaneLock
	DebugLog func(string, ...any)
}

// Compile-time assertion that *DefaultResolver satisfies NodeResolver.
var _ NodeResolver = (*DefaultResolver)(nil)

func (r *DefaultResolver) dbg(format string, args ...any) {
	if fn := r.DebugLog; fn != nil {
		fn(format, args...)
	}
}

// Resolve selects the best physical child of a synthetic node for the given order type.
func (r *DefaultResolver) Resolve(syntheticNode *nodes.Node, orderType protocol.OrderType, payloadCode string, binTypeID *int64) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(syntheticNode.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", syntheticNode.Name, err)
	}
	if len(children) == 0 {
		return nil, fmt.Errorf("synthetic node %s has no children", syntheticNode.Name)
	}

	// Delegate to group resolver for NGRP nodes
	if syntheticNode.NodeTypeCode == "NGRP" {
		gr := &GroupResolver{DB: r.DB, LaneLock: r.LaneLock, DebugLog: r.DebugLog}
		switch orderType {
		case protocol.OrderTypeRetrieve:
			return gr.ResolveRetrieve(syntheticNode, payloadCode)
		case protocol.OrderTypeStore:
			return gr.ResolveStore(syntheticNode, payloadCode, binTypeID)
		}
	}

	switch orderType {
	case protocol.OrderTypeRetrieve:
		node, err := r.resolveRetrieve(children, payloadCode)
		if err != nil {
			return nil, err
		}
		return &ResolveResult{Node: node}, nil
	case protocol.OrderTypeStore:
		node, err := r.resolveStore(children, payloadCode)
		if err != nil {
			return nil, err
		}
		return &ResolveResult{Node: node}, nil
	default:
		for _, c := range children {
			if c.Enabled {
				return &ResolveResult{Node: c}, nil
			}
		}
		return nil, fmt.Errorf("no enabled children for synthetic node %s", syntheticNode.Name)
	}
}

// resolveRetrieve finds the child node with the oldest unclaimed bin matching the payload code.
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
