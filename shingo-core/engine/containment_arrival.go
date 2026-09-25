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
// CALLED AFTER ApplyArrival at both arrival sites (the delivery path and
// the completion safety net). Deliberately NOT called on the multi-bin
// junction path: the divert's scope check requires the final dropoff to be
// the claim's FG outbound, and a multi-bin order is a supply leg to a
// consuming process node - it never diverts, so its arrivals are never
// containment arrivals.
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
	}
}
