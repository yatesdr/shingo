package domain

import "time"

// NodeProperty is a single key/value attached to a node. Properties
// are the extension point the graph uses for anything that doesn't
// warrant its own column — retrieve/store algorithm overrides,
// capacity hints, lane direction, edge-station bindings, etc. Keys
// are opaque strings owned by the consumer; the store layer treats
// them as data.
//
// In the store/nodes sub-package this type is aliased as
// nodes.Property. The domain name is the fully-qualified one so
// consumers reading domain code don't have to infer scope from the
// package.
type NodeProperty struct {
	ID        int64     `json:"id"`
	NodeID    int64     `json:"node_id"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	CreatedAt time.Time `json:"created_at"`
}
