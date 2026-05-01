package domain

import "time"

// TransitNodeName is the well-known name of the synthetic node that
// holds bins while they are physically in transit between their source
// and destination. Created by migration v15. Bins occupying this node
// have `is_synthetic=true` on the node row, which the existing
// is-synthetic-false filters in FindSourceFIFO, FindEmptyCompatible,
// and lane finders auto-exclude — so in-flight bins never get re-claimed
// by another order.
const TransitNodeName = "_TRANSIT"

// Node is any addressable location in the facility graph — physical
// storage slots, lanes, group-level aggregates, and synthetic routing
// parents. The node graph is a forest keyed by ParentID; Depth is the
// tree depth from the nearest root and is maintained by the store
// layer.
//
// The three Joined fields (NodeTypeCode, NodeTypeName, ParentName)
// are populated by every SELECT in store/nodes via LEFT JOINs on
// node_types and a self-join on nodes. They live here because every
// rendering path uses at least one of them and materialising them into
// the struct keeps call sites flat.
type Node struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	IsSynthetic bool      `json:"is_synthetic"`
	Zone        string    `json:"zone"`
	Enabled     bool      `json:"enabled"`
	Depth       *int      `json:"depth,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	NodeTypeID  *int64    `json:"node_type_id,omitempty"`
	ParentID    *int64    `json:"parent_id,omitempty"`
	// Joined fields
	NodeTypeCode string `json:"node_type_code,omitempty"`
	NodeTypeName string `json:"node_type_name,omitempty"`
	ParentName   string `json:"parent_name,omitempty"`
}
