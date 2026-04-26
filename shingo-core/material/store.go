package material

import (
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// Store is the narrow DB surface the material package depends on.
//
// Declaring it consumer-side does two things:
//
//  1. *store.DB satisfies it for free (Go interface satisfaction is
//     structural), so engine wiring does not change.
//  2. Tests can drop a hand-rolled fake into any material.* call and
//     exercise boundary walks, movement recording, and correction
//     recording without a database.
//
// The set below is exactly the methods the material functions call on
// the store — no more, no less. A lint of
// `grep 's\.' material.go` against the Store variable should match
// one-to-one with the entries here.
//
// Note on write path: CMS transactions themselves are persisted by
// *store.DB.CreateCMSTransactions, but that call lives in the engine
// wrapper (engine/cms_transactions.go), not in this package. Keeping
// the write out of Store is deliberate — material builds records;
// the engine is the boundary that writes and emits.
type Store interface {
	GetNode(id int64) (*nodes.Node, error)
	GetNodeProperty(nodeID int64, key string) string
	GetBin(id int64) (*bins.Bin, error)
	SumCatIDsAtBoundary(boundaryID int64) map[string]int64
}

// Compile-time check that *store.DB satisfies Store. If the store
// package drops or renames one of the methods above, this assertion
// catches it before the build fails somewhere further downstream.
var _ Store = (*store.DB)(nil)
