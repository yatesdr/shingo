package store

// Delegate file: the air-knife cleaning record lives in store/cleaning/. The
// engine's loop calls that package directly on db.DB, the way the confidence
// sampler calls robotconfidence.InsertBatch; these are the doors the read side
// and the opt-out write use.

import (
	"time"

	"shingocore/store/cleaning"
)

// CleaningSetup is the plant's one cleaning mission; ok is false when none has
// been saved.
func (db *DB) CleaningSetup() (cleaning.Setup, bool, error) {
	return cleaning.GetSetup(db.DB)
}

// CleaningViews is each robot's last dispatch and opt-out, one query.
func (db *DB) CleaningViews() (map[string]cleaning.View, error) {
	return cleaning.Views(db.DB)
}

// CleaningMissions lists missions dispatched since, newest first.
func (db *DB) CleaningMissions(since time.Time, limit int) ([]cleaning.Mission, error) {
	return cleaning.ListMissions(db.DB, since, limit)
}

// SetCleaningOptOut records or clears a robot's opt-out; changed is false when
// it was already so.
func (db *DB) SetCleaningOptOut(vehicleID string, out bool) (bool, error) {
	return cleaning.SetOptOut(db.DB, vehicleID, out)
}
