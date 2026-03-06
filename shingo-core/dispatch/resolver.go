package dispatch

import (
	"fmt"

	"shingocore/store"
)

// ResolveResult carries the resolved node and optionally a specific payload.
type ResolveResult struct {
	Node    *store.Node
	Payload *store.Payload // set when resolver identified a specific payload
}

// NodeResolver resolves a synthetic node to a physical child node.
type NodeResolver interface {
	Resolve(syntheticNode *store.Node, orderType string, blueprintID *int64, binTypeID *int64) (*ResolveResult, error)
}

// DefaultResolver resolves synthetic nodes using the database.
// For NGRP (node group) nodes, it delegates to the GroupResolver for two-level resolution.
type DefaultResolver struct {
	DB       *store.DB
	LaneLock *LaneLock
}

// Resolve selects the best physical child of a synthetic node for the given order type.
func (r *DefaultResolver) Resolve(syntheticNode *store.Node, orderType string, blueprintID *int64, binTypeID *int64) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(syntheticNode.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", syntheticNode.Name, err)
	}
	if len(children) == 0 {
		return nil, fmt.Errorf("synthetic node %s has no children", syntheticNode.Name)
	}

	// Delegate to group resolver for NGRP nodes
	if syntheticNode.NodeTypeCode == "NGRP" {
		gr := &GroupResolver{DB: r.DB, LaneLock: r.LaneLock}
		switch orderType {
		case OrderTypeRetrieve:
			return gr.ResolveRetrieve(syntheticNode, blueprintID)
		case OrderTypeStore:
			return gr.ResolveStore(syntheticNode, blueprintID, binTypeID)
		}
	}

	switch orderType {
	case OrderTypeRetrieve:
		node, err := r.resolveRetrieve(children, blueprintID)
		if err != nil {
			return nil, err
		}
		return &ResolveResult{Node: node}, nil
	case OrderTypeStore:
		node, err := r.resolveStore(children, blueprintID)
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

// resolveRetrieve finds the child node with the oldest unclaimed payload of the requested blueprint.
func (r *DefaultResolver) resolveRetrieve(children []*store.Node, blueprintID *int64) (*store.Node, error) {
	for _, child := range children {
		if !child.Enabled {
			continue
		}
		payloads, err := r.DB.ListPayloadsByNode(child.ID)
		if err != nil {
			continue
		}
		for _, p := range payloads {
			if p.ClaimedBy != nil || p.Status != "available" || p.BinStatus == "staged" {
				continue
			}
			if blueprintID != nil && p.BlueprintID != *blueprintID {
				continue
			}
			return child, nil
		}
	}
	return nil, fmt.Errorf("no child node has an available unclaimed payload")
}

// resolveStore finds the best child node for storage (consolidation-first, then emptiest).
func (r *DefaultResolver) resolveStore(children []*store.Node, blueprintID *int64) (*store.Node, error) {
	type candidate struct {
		node     *store.Node
		count    int
		hasMatch bool
	}

	var candidates []candidate
	for _, child := range children {
		if !child.Enabled || child.IsSynthetic {
			continue
		}
		count, err := r.DB.CountBinsByNode(child.ID)
		if err != nil {
			continue
		}
		inflight, _ := r.DB.CountActiveOrdersByDeliveryNode(child.Name)
		if count+inflight >= 1 {
			continue
		}

		hasMatch := false
		if blueprintID != nil {
			payloads, _ := r.DB.ListPayloadsByNode(child.ID)
			for _, p := range payloads {
				if p.BlueprintID == *blueprintID {
					hasMatch = true
					break
				}
			}
		}
		candidates = append(candidates, candidate{node: child, count: count, hasMatch: hasMatch})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no child node available for storage")
	}

	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.hasMatch && !best.hasMatch {
			best = c
		} else if c.hasMatch == best.hasMatch && c.count < best.count {
			best = c
		}
	}

	return best.node, nil
}
