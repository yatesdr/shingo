package domain

import "shingo/protocol"

// Trip is where an order's bin is on its trip: the one question a door asks
// before it ends a live order. Three answers, because the doors want three: an
// order whose bin has not left its pickup can be cancelled, a carried bin is
// set down first, and an order whose bin is set down has nothing left to stop.
//
// The order's row answers it when it can (TripFromRow); the rest is asked of
// the bins in one statement over a set of orders (store.DB.OrderTrips, its SQL
// helpers.OrderTripSQL).
type Trip int

const (
	// TripNotLifted: the bin has not left its pickup, or the order carries no
	// cart. A relay's cart set down before its final drop reads here too.
	TripNotLifted Trip = iota
	// TripCarried: a bin the order claims stands at _TRANSIT: a robot has lifted
	// it and not set it down.
	TripCarried
	// TripSetDown: the order reads delivered or later, or every cart it carries
	// stands on its final drop's node with the order's claim on it gone. The
	// landing puts the cart there and releases the claim before the order's
	// delivered write, so a landed order can still read in_transit.
	TripSetDown
)

// Left reports that the bin has left its pickup: carried or set down.
func (t Trip) Left() bool { return t != TripNotLifted }

func (t Trip) String() string {
	switch t {
	case TripCarried:
		return "carried"
	case TripSetDown:
		return "set down"
	default:
		return "not lifted"
	}
}

// TripFromRow is the half of the question the order's row answers, with no
// read: an order that reads delivered or confirmed has set its bin down (the
// set dispatch.IsPostDelivery names), and an order with no cart has none to
// lift. ok=false: the bins are asked (store.DB.OrderTrips).
func (o *Order) TripFromRow() (t Trip, ok bool) {
	switch {
	case o.Status == protocol.StatusDelivered || o.Status == protocol.StatusConfirmed:
		return TripSetDown, true
	case len(o.Carts) == 0:
		return TripNotLifted, true
	}
	return TripNotLifted, false
}
