package store

// Delegate for the Parts cross-aggregate reads (plan §3.E).

import (
	"time"

	"shingocore/store/parts"
)

func (db *DB) GetPartsProduced(since, until *time.Time, top int) ([]parts.Produced, error) {
	return parts.GetProduced(db.DB, since, until, top)
}

func (db *DB) GetPartsMissionDuration(since, until *time.Time, top int) ([]parts.MissionDuration, error) {
	return parts.GetMissionDuration(db.DB, since, until, top)
}

func (db *DB) GetPartsConsumption(since, until *time.Time, top int) ([]parts.Consumption, error) {
	return parts.GetConsumption(db.DB, since, until, top)
}
