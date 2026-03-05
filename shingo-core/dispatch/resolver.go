package dispatch

import (
	"fmt"

	"shingocore/store"
)

// ResolveResult carries the resolved node and optionally a specific instance.
type ResolveResult struct {
	Node     *store.Node
	Instance *store.PayloadInstance // set when resolver identified a specific instance
}

// NodeResolver resolves a synthetic node to a physical child node.
type NodeResolver interface {
	Resolve(syntheticNode *store.Node, orderType string, styleID *int64) (*ResolveResult, error)
}

// DefaultResolver resolves synthetic nodes using the database.
// For SMKT (supermarket) nodes, it delegates to the SupermarketResolver for two-level resolution.
type DefaultResolver struct {
	DB       *store.DB
	LaneLock *LaneLock
}

// Resolve selects the best physical child of a synthetic node for the given order type.
func (r *DefaultResolver) Resolve(syntheticNode *store.Node, orderType string, styleID *int64) (*ResolveResult, error) {
	children, err := r.DB.ListChildNodes(syntheticNode.ID)
	if err != nil {
		return nil, fmt.Errorf("list children of %s: %w", syntheticNode.Name, err)
	}
	if len(children) == 0 {
		return nil, fmt.Errorf("synthetic node %s has no children", syntheticNode.Name)
	}

	// Delegate to supermarket resolver if this is a SMKT with LANE children
	if syntheticNode.NodeTypeCode == "SMKT" {
		hasLanes := false
		for _, c := range children {
			if c.NodeTypeCode == "LANE" {
				hasLanes = true
				break
			}
		}
		if hasLanes {
			sr := &SupermarketResolver{DB: r.DB, LaneLock: r.LaneLock}
			switch orderType {
			case OrderTypeRetrieve:
				return sr.ResolveRetrieve(syntheticNode, styleID)
			case OrderTypeStore:
				return sr.ResolveStore(syntheticNode, styleID)
			}
		}
	}

	switch orderType {
	case OrderTypeRetrieve:
		node, err := r.resolveRetrieve(children, styleID)
		if err != nil {
			return nil, err
		}
		return &ResolveResult{Node: node}, nil
	case OrderTypeStore:
		node, err := r.resolveStore(children, styleID)
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

// resolveRetrieve finds the child node with the oldest unclaimed instance of the requested style.
func (r *DefaultResolver) resolveRetrieve(children []*store.Node, styleID *int64) (*store.Node, error) {
	for _, child := range children {
		if !child.Enabled {
			continue
		}
		instances, err := r.DB.ListInstancesByNode(child.ID)
		if err != nil {
			continue
		}
		for _, p := range instances {
			if p.ClaimedBy != nil || p.Status != "available" {
				continue
			}
			if styleID != nil && p.StyleID != *styleID {
				continue
			}
			return child, nil
		}
	}
	return nil, fmt.Errorf("no child node has an available unclaimed instance")
}

// resolveStore finds the best child node for storage (consolidation-first, then emptiest).
func (r *DefaultResolver) resolveStore(children []*store.Node, styleID *int64) (*store.Node, error) {
	type candidate struct {
		node     *store.Node
		count    int
		hasMatch bool
	}

	var candidates []candidate
	for _, child := range children {
		if !child.Enabled || child.Capacity <= 0 {
			continue
		}
		count, err := r.DB.CountInstancesByNode(child.ID)
		if err != nil {
			continue
		}
		if count >= child.Capacity {
			continue
		}

		hasMatch := false
		if styleID != nil {
			instances, _ := r.DB.ListInstancesByNode(child.ID)
			for _, p := range instances {
				if p.StyleID == *styleID {
					hasMatch = true
					break
				}
			}
		}
		candidates = append(candidates, candidate{node: child, count: count, hasMatch: hasMatch})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no child node has available capacity")
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
