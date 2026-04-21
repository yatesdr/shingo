package service

import (
	"shingocore/store"
)

// DemandService centralizes demand-row CRUD and produced-count
// mutations. Handlers call DemandService instead of reaching through
// engine passthroughs to *store.DB.
//
// Absorbed from engine_db_methods.go as part of the Phase 3a closeout
// (PR 3a.6). Methods are thin delegates today — any validation or
// composite flow lands here in a later phase without disturbing the
// handler layer.
type DemandService struct {
	db *store.DB
}

func NewDemandService(db *store.DB) *DemandService {
	return &DemandService{db: db}
}

// --- CRUD -----------------------------------------------------------------

// Create inserts a new demand row and returns its ID.
func (s *DemandService) Create(catID, description string, demandQty int64) (int64, error) {
	return s.db.CreateDemand(catID, description, demandQty)
}

// Get loads a demand by ID.
func (s *DemandService) Get(id int64) (*store.Demand, error) {
	return s.db.GetDemand(id)
}

// Update persists field changes on a demand row.
func (s *DemandService) Update(id int64, catID, description string, demandQty, producedQty int64) error {
	return s.db.UpdateDemand(id, catID, description, demandQty, producedQty)
}

// UpdateAndResetProduced updates the demand row and resets the
// produced_qty counter to zero. Used by the "apply" flow when a new
// demand cycle begins.
func (s *DemandService) UpdateAndResetProduced(id int64, description string, demandQty int64) error {
	return s.db.UpdateDemandAndResetProduced(id, description, demandQty)
}

// Delete removes a demand row.
func (s *DemandService) Delete(id int64) error {
	return s.db.DeleteDemand(id)
}

// List returns every demand row.
func (s *DemandService) List() ([]*store.Demand, error) {
	return s.db.ListDemands()
}

// --- Produced counters ----------------------------------------------------

// SetProduced overwrites the produced_qty field on a demand.
func (s *DemandService) SetProduced(id int64, qty int64) error {
	return s.db.SetProduced(id, qty)
}

// ClearProduced resets the produced_qty field on a single demand.
func (s *DemandService) ClearProduced(id int64) error {
	return s.db.ClearProduced(id)
}

// ClearAllProduced resets produced_qty across every demand row.
func (s *DemandService) ClearAllProduced() error {
	return s.db.ClearAllProduced()
}

// --- Production log -------------------------------------------------------

// ListProductionLog returns the most recent production-log entries for
// a catalogue ID, capped at limit rows.
func (s *DemandService) ListProductionLog(catID string, limit int) ([]*store.ProductionLogEntry, error) {
	return s.db.ListProductionLog(catID, limit)
}
