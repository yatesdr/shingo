//go:build docker

package store_test

import (
	"shingocore/store"
	"shingocore/store/reservations"
)

// scannerReserve is the scanner's soft reserve and pointer as it stands
// (fulfillment.Scanner, the reserve arm: reservations.AcquireAndPoint, one
// statement), with between run where another transaction can land: after it.
func scannerReserve(db *store.DB, orderID, binID int64, between func()) error {
	if err := reservations.AcquireAndPoint(db.DB, orderID, orderID, binID, "reserveForDispatch"); err != nil {
		return err
	}
	between()
	return nil
}
