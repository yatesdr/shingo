package store

// Delegate for the Parts cross-aggregate reads (plan §3.E).

import (
	"time"

	"shingocore/store/parts"
)

func (db *DB) GetPartsProduced(since, until *time.Time, top int) ([]parts.Produced, error) {
	return parts.GetProduced(db.DB, since, until, top)
}

func (db *DB) GetPartsCycleTime(since, until *time.Time, top int) ([]parts.Cycle, error) {
	return parts.GetCycleTime(db.DB, since, until, top)
}

func (db *DB) GetPartsConsumption(since, until *time.Time, top int) ([]parts.Consumption, error) {
	return parts.GetConsumption(db.DB, since, until, top)
}
