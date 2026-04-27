package domain

// NodeTileState holds summary flags for rendering a node tile in the
// nodes-overview UI. Each flag is computed by aggregating bins at the
// node — HasPayload is true when any bin at the node has a confirmed
// payload, HasEmptyBin is true when any bin is empty or unconfirmed,
// Claimed/Staged/Maintenance reflect bin-level lifecycle state.
//
// The struct is intentionally a derived view, not a persisted row;
// store/bins.NodeTileStates(db) computes the map by aggregating
// bins. Lives in domain/ because www handlers reference the type as
// part of the page-data response and shouldn't need to import the
// bins sub-package for it. The store/bins package re-exports the
// type via `type NodeTileState = domain.NodeTileState`.
type NodeTileState struct {
	HasPayload  bool // bin with a confirmed payload
	HasEmptyBin bool // bin with no payload or unconfirmed manifest
	Claimed     bool
	Staged      bool
	Maintenance bool // bin in maintenance or flagged
}
