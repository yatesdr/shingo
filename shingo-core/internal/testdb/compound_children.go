package testdb

import (
	"testing"

	"shingocore/store"
	"shingocore/store/orders"
)

// CreateCompoundChildren writes a compound's children for a test, the way the
// dig's issue does on its own transaction (dispatch writeCompoundChildren):
// the compound opened under its parent (store.OpenCompoundTx), then each
// child's row and its own acts after the row (store.HoldLegTx). It writes the
// rows with the store's INSERT, not the issue, since a fixture's children are
// already whole orders; the issue's own legs are dispatch's to pin. It returns
// the displaced orders and logs the steals once committed, as the dig does.
func CreateCompoundChildren(_ testing.TB, db *store.DB, children []store.CompoundChild, lanes *store.CompoundLanes) ([]store.DisplacedForCart, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	parentID := int64(0)
	for _, c := range children {
		if c.Order != nil && c.Order.ParentOrderID != nil {
			parentID = *c.Order.ParentOrderID
			break
		}
	}
	if parentID != 0 || lanes != nil {
		if err := store.OpenCompoundTx(tx, parentID, lanes); err != nil {
			return nil, err
		}
	}
	var displaced []store.DisplacedForCart
	var stolen store.Steals
	for _, c := range children {
		if err := orders.Create(tx, c.Order); err != nil {
			return nil, err
		}
		byHand, took, err := store.HoldLegTx(tx, c)
		if err != nil {
			return nil, err
		}
		displaced, stolen = append(displaced, byHand...), append(stolen, took...)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	store.LogSteals(stolen)
	return displaced, nil
}
