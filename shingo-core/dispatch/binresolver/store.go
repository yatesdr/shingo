package binresolver

import (
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
)

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
	// Node / child listing. ListChildNodesUnlocked is the candidate read for
	// every group scan: it excludes dig-held lanes in the query, so a locked
	// lane is never a candidate rather than being a candidate this package has
	// to remember to skip. ListChildNodes stays for resolver.go's synthetic-node
	// walk, which is not a lane scan.
	//
	// "Dig-held" is asked ON BEHALF OF the asker — a dig does not exclude the
	// order it is being run for. The parameter is not a convenience; without it
	// an expose dig hides its own uncovered bin from its own parent. See
	// store/reservations/dig_exclusion.go.
	ListChildNodes(parentID int64) ([]*nodes.Node, error)
	ListChildNodesUnlocked(parentID int64, asker reservations.DigAsker) ([]*nodes.Node, error)
	GetNode(id int64) (*nodes.Node, error)
	GetNodeProperty(nodeID int64, key string) string

	// Bin state at a node (for non-lane children).
	ListBinsByNode(nodeID int64) ([]*bins.Bin, error)
	CountBinsByNode(nodeID int64) (int, error)

	// ── The maintained-group level (MG4-1) ──────────────────────────────────
	//
	// ListMaintainLevels is the declared CAP; CountEmptyBinsOfTypeInGroup is what
	// the group is actually holding. The filter refuses a store into a group that
	// has reached its level, so the two are read together.
	//
	// THE COUNT IS THE FROZEN PREDICATE. It is the same query the level keeper
	// subtracts from — bins.CountEmptyOfTypeInGroup, sharing its WHERE with the
	// finder by construction — and it is read here rather than recomputed. A
	// resolver that counted the group its own way would be a second answer to
	// "how full is this group", and the keeper and the resolver disagreeing about
	// that is the whole feature quietly not working.
	ListMaintainLevels(groupNodeID int64) ([]nodes.MaintainLevel, error)
	CountEmptyBinsOfTypeInGroup(binTypeCode string, groupNodeID int64) (int, error)

	// In-flight orders (used for storage candidate screening).
	CountActiveOrdersByDeliveryNode(nodeName string) (int, error)

	// Lane-aware queries.
	ListLaneSlots(laneID int64) ([]*nodes.Node, error)
	CountBinsInLane(laneID int64) (int, error)
	FindSourceBinInLane(laneID int64, payloadCode string) (*bins.Bin, error)
	FindStoreSlotInLane(laneID int64) (*nodes.Node, error)
	// FindStoreSlotInLaneExcluding is the OWNER-AWARE form, and the group
	// resolvers use it so a re-resolve can see the slot the asking order already
	// holds. excludeOrderID 0 reproduces the blind form exactly (order ids are
	// positive), which is why reservations.Anyone leaves every existing caller
	// byte-identical. See nodes.FindStoreSlotInLaneExcluding for why the blind
	// form is unusable on a re-resolve: an order matches its own claim, its own
	// reservation and its own delivery_node, so its slot is invisible to it and
	// the resolver hands back the next-best one, which is SHALLOWER.
	FindStoreSlotInLaneExcluding(laneID, excludeOrderID int64) (*nodes.Node, error)
	FindOldestBuriedBin(laneID int64, payloadCode string) (*bins.Bin, *nodes.Node, error)
	FindBuriedBin(laneID int64, payloadCode string) (*bins.Bin, *nodes.Node, error)

	// LaneAcceptsInbound reports whether a lane's mouth currently has no hold
	// that conflicts with an inbound store (empty or inbound-only). The read
	// behind resolve-around; called only when a group enables the arm.
	LaneAcceptsInbound(laneID int64) (bool, error)

	// Effective constraint sets (payloads + bin types allowed at a node,
	// resolved through whatever inheritance rules the node graph uses).
	GetEffectivePayloads(nodeID int64) ([]*payloads.Payload, error)
	GetEffectiveBinTypes(nodeID int64) ([]*bins.BinType, error)
}

// Compile-time check that *store.DB satisfies Store. If the store package
// drops or renames one of the methods above, this assertion catches it
// before the build fails somewhere further downstream.
var _ Store = (*store.DB)(nil)
