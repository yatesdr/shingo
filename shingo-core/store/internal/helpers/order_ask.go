package helpers

import (
	"fmt"

	"shingo/protocol"
)

// ── WHERE AN ORDER STANDS AGAINST THE FLEET: ONE SPELLING PER QUESTION ──────
//
// An order's status says where it rests; orders.asked_vendor_order_id says
// whether Core is asking the fleet for it right now. An order with an open ask
// has taken its holds and is waiting on the fleet's answer, whatever its status
// reads, so it is spoken for: no reader may treat it as resting, and no door may
// ask for it again. Every reader of "has Core sent it" reads both columns, here,
// and the raw status lists these are built on are banned outside this file
// (.golangci.yml, the ask guard). The Go forms are domain/order_ask.go.
//
// alias is the orders table alias ("" for an unaliased `orders`).

// AskedCol is the open ask's column.
const AskedCol = "asked_vendor_order_id"

func aliasDot(alias string) string {
	if alias == "" {
		return ""
	}
	return alias + "."
}

// AcquiringSQL: Core may ask the fleet for the order now. Born and not yet
// tried, or tried and waiting, and not being asked for already. The fulfillment
// scanner's population and every sweep of orders that may still be tried.
func AcquiringSQL(alias string) string {
	q := aliasDot(alias)
	return fmt.Sprintf("(%sstatus IN (%s) AND %s%s = '')", q, protocol.AcquiringStatusSQLList(), q, AskedCol)
}

// PreDispatchSQL: the order rests before the fleet, holding nothing hard for a
// job. pending or queued, with no ask open.
func PreDispatchSQL(alias string) string {
	q := aliasDot(alias)
	return fmt.Sprintf("(%sstatus IN (%s) AND %s%s = '')", q, protocol.PreDispatchStatusSQLList(), q, AskedCol)
}

// PastPreDispatchSQL is PreDispatchSQL's complement: the order is spoken for.
// Core is asking for it, or the fleet has it, or it has ended; a reader that
// wants only live orders adds its own terminal clause.
func PastPreDispatchSQL(alias string) string {
	return "NOT " + PreDispatchSQL(alias)
}

// AwaitingFleetSQL is orders.AwaitingFleetSQL's one spelling — see it for why
// vendor_order_id is the authority and the status set is derived. An open ask
// is not awaiting: Core is already asking. It lives here because the burial
// guard's holder arm reads it, and this package cannot import store/orders.
func AwaitingFleetSQL(alias string) string {
	q := aliasDot(alias)
	return fmt.Sprintf("%sstatus IN (%s) AND COALESCE(%svendor_order_id, '') = '' AND COALESCE(%s%s, '') = ''",
		q, protocol.PreDispatchStatusSQLList(), q, q, AskedCol)
}
