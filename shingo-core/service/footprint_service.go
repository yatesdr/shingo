package service

import (
	"time"

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

// Get returns the plant-footprint summary. loc is the plant timezone used to
// key the daily load/unload velocity buckets.
func (s *FootprintService) Get(loc *time.Location) (*footprint.Footprint, error) {
	return s.db.GetFootprint(loc)
}
