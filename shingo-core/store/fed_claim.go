package store

import "shingocore/store/bins"

// fed_claim.go — where a node stands in a claim's cell, on *store.DB: a line
// position, a fed press's outside, the press a pull to that outside is for,
// and where its claim draws from. The statements are bins' (fed_claim.go,
// bins.go's IsLinePosition); these are their store doors, in one file.

// IsLinePosition reports whether a node is a claim's line position — its line,
// paired or second paired node: the predicate the empty search excludes a
// carrier by. A carrier standing there is that position's own, never a fetch
// to send elsewhere.
func (db *DB) IsLinePosition(nodeName string) (bool, error) {
	return bins.IsLinePosition(db.DB, nodeName)
}

// IsFedOutside reports whether a node is a fed press cell's outside position
// (bins.FedOutsideSQL): where a tote cart's count is exact only at its lift.
func (db *DB) IsFedOutside(nodeName string) (bool, error) {
	return bins.IsFedOutside(db.DB, nodeName)
}

// FedPressFor is the press whose outside outside is and whose claim carries
// part, one read (bins.FedPressFor): a tops & bottoms pull's asker.
func (db *DB) FedPressFor(part, outside string) (string, error) {
	return bins.FedPressFor(db.DB, part, outside)
}

// FedInboundSource is where the active fed claim at a station's core node draws
// from, asked about one of its positions, and the episode kind of the asking
// order's origin. See bins.FedInboundSource.
func (db *DB) FedInboundSource(stationID, coreNodeName, position, originID string) (src, kind string, err error) {
	return bins.FedInboundSource(db.DB, stationID, coreNodeName, position, originID)
}
