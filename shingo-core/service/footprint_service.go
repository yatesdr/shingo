package service

import (
	"shingocore/store"
	"shingocore/store/footprint"
)

// FootprintService serves the Operations Overview "Plant Footprint" section
// (plan §15.D): cells/bins under management and the load/unload velocity
// rhythm. A thin read-only surface over store/footprint; handlers reach it
// via the engine rather than importing store directly (www-no-direct-store
// depguard).
type FootprintService struct {
	db *store.DB
}

func NewFootprintService(db *store.DB) *FootprintService {
	return &FootprintService{db: db}
}

// Get returns the plant-footprint summary.
func (s *FootprintService) Get() (*footprint.Footprint, error) {
	return s.db.GetFootprint()
}
