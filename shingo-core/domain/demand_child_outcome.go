// demand_child_outcome.go — the read shape behind Stage 5.2's cause column.
//
// Here rather than in store/ for the depguard reason the rest of the demand
// grain is: www may not import shingocore/store, so a row shape a handler names
// has to be declared where a handler can reach it. store/ aliases it.

package domain

// ChildStatusCount is how many of one episode's child orders sit at one status,
// split by whether the fleet vendor ever acknowledged them.
//
// IT IS RAW, AND THAT IS THE POINT. This carries no notion of "consumed",
// "dying" or "churn" — those are display classifications, they change as the
// protocol grows, and they belong in one tested pure function rather than
// spread between a SQL CASE and the Go that renders it.
type ChildStatusCount struct {
	OriginID string

	// Status is the order's CURRENT status, as a raw string. Not protocol.Status
	// deliberately: the whole reason the classifier has an "unrecognised" bucket
	// is that a row on disk can carry a value this build does not know, and
	// typing it as the enum here would suggest a guarantee the database does not
	// make.
	Status string

	// ReachedVendor is whether vendor_order_id is non-empty, i.e. whether the
	// fleet vendor ever acknowledged this order.
	//
	// THIS IS THE ONLY PRE-DISPATCH DISCRIMINATOR THE SCHEMA HAS. There is no
	// dispatched_at, no cancelled_at and no cancel_reason on `orders`, and a
	// cancelled order's current status is `cancelled` — so nothing else on the
	// row can say which side of dispatch a cancel happened on. It is sound in
	// one direction only: false proves the vendor never had it, but false does
	// NOT prove the cancel was a re-arm. See www.ClassifyChild, which names its
	// categories after that limit instead of past it.
	ReachedVendor bool

	Count int
}
