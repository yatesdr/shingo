package engine

import "shingocore/store/nodes"

// containment_arrival.go - the divert path's arrival stamp.
//
// The divert (dispatch's placeForContainment) re-points an FG delivery to
// the payload's containment destination at dispatch time, but until now
// nothing stamped the bin on landing. Every anti-resourcing rule Core owns
// keys on bins.quality_hold, so a diverted bin standing in the containment
// area was, to the sourcing engine, just another full bin of its payload -
// and the plant-wide retrieve fallback (FindSourceFIFO, which scans every
// node in the plant) would have handed it straight back into production.
// The station-hold path stamps its marker at hold time and the recall
// stamps as it walks bins in; this is the third leg, so all three shapes
// of "a bin that is contained" carry the same marker and read the same to
// everything downstream.
//
// CALLED AFTER every arrival: the delivery path and the completion safety
// net (a cart set down last), and the intermediate store (a cart set down at
// an interior drop, handleStoreBlockCompleted). The divert moves the drop of
// the carrier the order lifts at its press, which is not always the plan's
// last: an index press's Robot 1 takes the full to containment and goes on
// to fill the back position, so the full lands at an interior drop, and the
// finished path, which skips a cart already at its drop, never sees it. The
// stamp asks of every arrival whether the bin's part is contained and the
// node is its containment destination, so an arrival anywhere else is a
// no-op.
//
// Failures log and return; the arrival itself has already committed and
// must not be unwound by a stamping problem. A missed stamp is the bug
// this file exists to close, so it is loud in the journal - but a missed
// stamp leaves the bin where the divert put it, inside a containment area
// whose positions are claim-less, which no scoped need ever names: the
// window it opens is the plant-wide fallback only.
func (e *Engine) stampContainmentArrival(binID int64, destNode *nodes.Node, payloadCode, site string) {
	if destNode == nil || payloadCode == "" || binID <= 0 {
		return
	}
	stamped, err := e.db.StampContainmentArrival(binID, destNode.ID, payloadCode, "containment-divert")
	if err != nil {
		e.logFn("engine: containment arrival stamp for bin %d at %s (%s): %v", binID, destNode.Name, site, err)
		return
	}
	if stamped {
		e.logFn("containment: bin %d arrived at %s (payload %s is contained, %s) - hold marker stamped",
			binID, destNode.Name, payloadCode, site)
		e.notifyContainmentChanged()
	}
}
