package binresolver

import "shingocore/store"

// Store is the narrow DB surface that the bin resolvers depend on.
//
// Declaring it here (consumer-side) does two things:
//
//  1. *store.DB satisfies it for free (Go interface satisfaction is
//     structural), so production wiring in engine/ does not change.
//  2. Tests can drop a hand-rolled fake into DefaultResolver.DB /
//     GroupResolver.DB and exercise FIFO / COST / FAVL / LKND / DPTH
//     algorithms without spinning up a database.
//
// The set below is exactly the methods the resolver files in this
// package call on *store.DB — no more, no less. A lint of
// `grep 'r\.DB\.' *.go` should match one-to-one with the entries here.
type Store interface {
	// Node / child listing.
	ListChildNodes(parentID int64) ([]*store.Node, error)
	GetNode(id int64) (*store.Node, error)
	GetNodeProperty(nodeID int64, key string) string

	// Bin state at a node (for non-lane children).
	ListBinsByNode(nodeID int64) ([]*store.Bin, error)
	CountBinsByNode(nodeID int64) (int, error)

	// In-flight orders (used for storage candidate screening).
	CountActiveOrdersByDeliveryNode(nodeName string) (int, error)

	// Lane-aware queries.
	ListLaneSlots(laneID int64) ([]*store.Node, error)
	CountBinsInLane(laneID int64) (int, error)
	FindSourceBinInLane(laneID int64, payloadCode string) (*store.Bin, error)
	FindStoreSlotInLane(laneID int64) (*store.Node, error)
	FindOldestBuriedBin(laneID int64, payloadCode string) (*store.Bin, *store.Node, error)
	FindBuriedBin(laneID int64, payloadCode string) (*store.Bin, *store.Node, error)

	// Effective constraint sets (payloads + bin types allowed at a node,
	// resolved through whatever inheritance rules the node graph uses).
	GetEffectivePayloads(nodeID int64) ([]*store.Payload, error)
	GetEffectiveBinTypes(nodeID int64) ([]*store.BinType, error)
}

// Compile-time check that *store.DB satisfies Store. If the store package
// drops or renames one of the methods above, this assertion catches it
// before the build fails somewhere further downstream.
var _ Store = (*store.DB)(nil)
