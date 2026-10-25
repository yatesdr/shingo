package store

import "shingocore/store/bins"

// fed_claim.go — where a node stands in a claim's cell, on *store.DB: a line
// position, a fed press's outside, the carrier its pull lands there, and where
// its claim draws from. The statements are bins' (fed_claim.go, tote_carrier.go,
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

// ToteCarrierFor is the press whose outside outside is and the tote carrier of
// part, one read (bins.ToteCarrierFor): a tops & bottoms pull's.
func (db *DB) ToteCarrierFor(part, outside string) (bins.ToteCarrier, error) {
	return bins.ToteCarrierFor(db.DB, part, outside)
}

// FedInboundSource is where the active fed claim at a station's core node draws
// from, asked about one of its positions. See bins.FedInboundSource.
func (db *DB) FedInboundSource(stationID, coreNodeName, position string) (string, error) {
	return bins.FedInboundSource(db.DB, stationID, coreNodeName, position)
}
