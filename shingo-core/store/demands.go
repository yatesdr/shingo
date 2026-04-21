package store

// Phase 5 delegate file: demand + production-log persistence lives in
// store/demands/. This file preserves the *store.DB method surface so
// external callers don't need to change.

import (
	"shingocore/store/demands"
)

// Type aliases preserve the store.Demand and ProductionLogEntry public API.
type Demand = demands.Demand
type ProductionLogEntry = demands.ProductionLogEntry

func (db *DB) CreateDemand(catID, description string, demandQty int64) (int64, error) {
	return demands.Create(db.DB, catID, description, demandQty)
}

func (db *DB) UpdateDemand(id int64, catID, description string, demandQty, producedQty int64) error {
	return demands.Update(db.DB, id, catID, description, demandQty, producedQty)
}

func (db *DB) UpdateDemandAndResetProduced(id int64, description string, demandQty int64) error {
	return demands.UpdateAndResetProduced(db.DB, id, description, demandQty)
}

func (db *DB) DeleteDemand(id int64) error { return demands.Delete(db.DB, id) }

func (db *DB) ListDemands() ([]*Demand, error) { return demands.List(db.DB) }

func (db *DB) GetDemand(id int64) (*Demand, error) { return demands.Get(db.DB, id) }

func (db *DB) GetDemandByCatID(catID string) (*Demand, error) {
	return demands.GetByCatID(db.DB, catID)
}

func (db *DB) IncrementProduced(catID string, qty int64) error {
	return demands.IncrementProduced(db.DB, catID, qty)
}

func (db *DB) ClearAllProduced() error { return demands.ClearAllProduced(db.DB) }

func (db *DB) ClearProduced(id int64) error { return demands.ClearProduced(db.DB, id) }

func (db *DB) SetProduced(id int64, qty int64) error { return demands.SetProduced(db.DB, id, qty) }

func (db *DB) LogProduction(catID, stationID string, qty int64) error {
	return demands.LogProduction(db.DB, catID, stationID, qty)
}

func (db *DB) ListProductionLog(catID string, limit int) ([]*ProductionLogEntry, error) {
	return demands.ListProductionLog(db.DB, catID, limit)
}
