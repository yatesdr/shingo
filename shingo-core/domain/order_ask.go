package domain

import "shingo/protocol"

// ── WHERE AN ORDER STANDS AGAINST THE FLEET: ONE SPELLING PER QUESTION ──────
//
// An order's status says where it rests; AskedVendorOrderID says whether Core
// is asking the fleet for it right now. An order with an open ask has taken its
// holds and waits on the fleet's answer, whatever its status reads, so it is
// spoken for: no reader may treat it as resting, and no door may ask for it
// again. These are the Go forms of store/internal/helpers/order_ask.go; the raw
// status predicates they are built on are banned outside the two files
// (.golangci.yml, the ask guard).

// AskOpen reports Core asking the fleet to create this order's job now.
func (o *Order) AskOpen() bool { return o.AskedVendorOrderID != "" }

// Acquiring reports that Core may ask the fleet for this order now: born and
// not yet tried, or tried and waiting, and not being asked for already.
func (o *Order) Acquiring() bool { return Acquiring(o.Status, o.AskedVendorOrderID) }

// PreDispatch reports the order resting before the fleet, holding nothing hard
// for a job: pending or queued, with no ask open. Its negation is "spoken for".
func (o *Order) PreDispatch() bool { return PreDispatch(o.Status, o.AskedVendorOrderID) }

// AwaitingFleet reports the order at rest and never handed to the fleet: the Go
// form of orders.AwaitingFleetSQL.
func (o *Order) AwaitingFleet() bool { return o.PreDispatch() && o.VendorOrderID == "" }

// Acquiring is the method's question for a reader holding a status and an ask
// but no Order (soakstat's samples).
func Acquiring(s protocol.Status, asked string) bool {
	return protocol.IsAcquiring(s) && asked == ""
}

// PreDispatch is the method's question for a reader holding a status and an ask
// but no Order.
func PreDispatch(s protocol.Status, asked string) bool {
	return protocol.IsPreDispatch(s) && asked == ""
}
