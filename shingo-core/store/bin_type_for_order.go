package store

// BinTypeForOrder answers ONE question: which bin type is this order
// going to put down. It exists because the per-node Allowed Bin Types gate had
// no way to find out.
//
// ── WHY THIS IS A LOOKUP AND NOT A PARAMETER ────────────────────────────────
//
// The gate in binresolver.binTypeAllowed has been correct since it was written
// and did nothing for just as long, because the bin type reached it as a
// parameter that five of seven call sites passed as nil. A parameter threaded
// through every path is a check that every NEW path forgets, and the history
// here is the proof: the one caller that did pass it (the level keeper) was
// added years after the gate and was the first to exercise it.
//
// So the resolver derives the answer itself, once, from the order id it already
// holds in the dig asker. Nothing has to be threaded and nothing can be missed:
// every store resolution — simple move, retrieve, complex step, lane-gate
// widening, dug-lane redirect — is typed by construction.
//
// ── THREE SOURCES, IN DESCENDING CERTAINTY ──────────────────────────────────
//
// The same question is asked at three different moments in an order's life, and
// only the last of them has a carrier in hand:
//
//  1. orders.bin_id — the bin type is chosen and recorded. Every simple move and
//     retrieve that has reached dispatch is here. This is the physical thing the
//     robot is holding, so it outranks anything inferred.
//  2. the order's BIN reservations — a complex order's legs hold their carriers
//     before bin_id means anything, so a dropoff step resolving into a group
//     reads the hold its paired pickup took.
//  3. nothing — the caller's own parameter covers the pre-carrier case (a level
//     keeper ask names a type before any bin exists) and stays an override.
//
// ── EVERY DOUBT RETURNS NIL, WHICH MEANS "DO NOT NARROW" ────────────────────
//
// Not "refuse". An unreadable row, an order with no bin type yet, a plan holding
// two carriers of different types — all of them resolve untyped, which is the
// behaviour every order in the plant had before this function existed. The
// alternative is refusing to place a carrier because we could not work out what
// it was, which converts a transient read fault into a parked robot.
//
// AMBIGUITY IS NIL ON PURPOSE. An order holding two bin types has no single
// answer, and choosing either would fence the resolve on a guess. Two held types
// is a compound or a mis-shaped plan; both deserve the untyped resolve they have
// always had rather than a narrowing nobody asked for.
//
// The error is returned rather than swallowed so the resolver can say a read
// failed. A silent nil on a database fault is how an un-narrowed placement
// becomes unexplainable after the fact.
func (db *DB) BinTypeForOrder(orderID int64) (*int64, error) {
	if orderID == 0 {
		return nil, nil
	}
	order, err := db.GetOrder(orderID)
	if err != nil {
		return nil, err
	}
	if order == nil {
		return nil, nil
	}

	if order.BinID != nil {
		bin, err := db.GetBin(*order.BinID)
		if err != nil {
			return nil, err
		}
		if bin != nil {
			id := bin.BinTypeID
			return &id, nil
		}
	}

	return db.heldBinType(orderID)
}

// heldBinType reads the bin type off the order's bin reservations, for
// the orders whose bin_id is not the answer — a complex order's legs hold their
// carriers well before the column means anything.
//
// SLOT AND MOUTH ROWS ARE SKIPPED, not treated as unknown: ListByOrder returns
// both kinds and sets exactly one of BinID/NodeID, so a zero BinID is a row
// about a PLACE, which names no bin type and says nothing either way.
func (db *DB) heldBinType(orderID int64) (*int64, error) {
	held, err := db.ListReservationsByOrder(orderID)
	if err != nil {
		return nil, err
	}
	var found *int64
	for _, r := range held {
		if r.BinID == 0 {
			continue
		}
		bin, err := db.GetBin(r.BinID)
		if err != nil {
			return nil, err
		}
		if bin == nil {
			// A hold naming a bin that no longer exists makes the set unknowable
			// rather than empty, and those are different facts.
			return nil, nil
		}
		if found != nil && *found != bin.BinTypeID {
			return nil, nil // two types held: no single answer
		}
		id := bin.BinTypeID
		found = &id
	}
	return found, nil
}
