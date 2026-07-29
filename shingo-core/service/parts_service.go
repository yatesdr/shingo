package service

import (
	"time"

	"shingocore/store"
	"shingocore/store/parts"
)

// PartsService serves the dashboard "Parts" section (plan §3.E): parts
// produced, cycle time, and consumption. Read-only cross-aggregate surface
// over store/parts; reached via the engine (www-no-direct-store depguard).
type PartsService struct {
	db *store.DB
}

func NewPartsService(db *store.DB) *PartsService {
	return &PartsService{db: db}
}

func (s *PartsService) Produced(since, until *time.Time, top int) ([]parts.Produced, error) {
	return s.db.GetPartsProduced(since, until, top)
}

// MissionDuration is how long the missions carrying a part took — NOT its cycle
// time. See parts.MissionDuration for why the two are different measurements and
// why only one of them gets the word.
func (s *PartsService) MissionDuration(since, until *time.Time, top int) ([]parts.MissionDuration, error) {
	return s.db.GetPartsMissionDuration(since, until, top)
}

func (s *PartsService) Consumption(since, until *time.Time, top int) ([]parts.Consumption, error) {
	return s.db.GetPartsConsumption(since, until, top)
}
